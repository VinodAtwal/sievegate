package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"apimigrate/config"
	"apimigrate/models"
)

// sentinel replaces values at ignored paths so both sides compare equal
// regardless of the actual value at that location.
const sentinel = "__APIMIGRATE_IGNORED__"

// Compare runs the full comparison between an original and migrated response.
func Compare(r requestInfo, orig, mig capturedResponse, cfg *config.Config, runID string) *models.Comparison {
	c := &models.Comparison{
		RunID:               runID,
		Method:              r.method,
		Path:                r.path,
		Query:               r.query,
		StatusOriginal:      orig.status,
		StatusMigrated:      mig.status,
		DurationOriginal:    orig.duration,
		DurationMigrated:    mig.duration,
		SizeOriginal:        len(orig.body),
		SizeMigrated:        len(mig.body),
		ErrorOriginal:       orig.errString(),
		ErrorMigrated:       mig.errString(),
		HeaderMismatches:    compareHeaders(orig.header, mig.header, cfg.HeadersToCompare),
		BodyPreviewOriginal: preview(orig.body, cfg.BodyPreviewChars),
		BodyPreviewMigrated: preview(mig.body, cfg.BodyPreviewChars),
	}

	c.StatusMatch = c.ErrorOriginal == "" && c.ErrorMigrated == "" && orig.status == mig.status
	c.HeaderMatch = c.ErrorOriginal == "" && c.ErrorMigrated == "" && len(c.HeaderMismatches) == 0
	c.BodyMatch = c.ErrorOriginal == "" && c.ErrorMigrated == "" && bodiesMatch(orig.body, mig.body, cfg.IgnoreFields)

	if !c.BodyMatch {
		c.DiffPaths = findDiffPaths(orig.body, mig.body, cfg.IgnoreFields, "", 20)
	}

	c.IsMatch = c.StatusMatch && c.HeaderMatch && c.BodyMatch
	return c
}

type requestInfo struct {
	method string
	path   string
	query  string
}

func (c capturedResponse) errString() string {
	if c.err == nil {
		return ""
	}
	return c.err.Error()
}

// compareHeaders compares only the configured header names. When the config
// list is empty every response header is compared. Header names are matched
// case-insensitively.
func compareHeaders(orig, mig headerMap, configured []string) []string {
	var mismatches []string
	for _, name := range configured {
		ov := orig.get(name)
		mv := mig.get(name)
		if ov != mv {
			mismatches = append(mismatches, fmt.Sprintf("%s (orig=%q mig=%q)", name, ov, mv))
		}
	}
	return mismatches
}

// bodiesMatch compares two response bodies. When both parse as JSON the
// comparison is structural and ignores configured fields. Otherwise it falls
// back to a raw byte comparison.
func bodiesMatch(orig, mig []byte, ignoreFields []string) bool {
	ov, okO := parseJSON(orig)
	mv, okM := parseJSON(mig)
	if okO && okM {
		ov = stripIgnored(ov, ignoreFields)
		mv = stripIgnored(mv, ignoreFields)
		return reflect.DeepEqual(ov, mv)
	}
	if okO != okM {
		return false
	}
	return bytes.Equal(trimSpace(orig), trimSpace(mig))
}

// findDiffPaths locates the structural differences between two JSON bodies,
// honoring ignored fields and returning at most limit paths.
func findDiffPaths(orig, mig []byte, ignoreFields []string, prefix string, limit int) []string {
	ov, okO := parseJSON(orig)
	mv, okM := parseJSON(mig)
	if !okO || !okM {
		return nil
	}
	ov = stripIgnored(ov, ignoreFields)
	mv = stripIgnored(mv, ignoreFields)

	var out []string
	collect(ov, mv, "$", &out, &limit)
	return out
}

func collect(orig, mig interface{}, prefix string, out *[]string, limit *int) {
	if *limit <= 0 {
		return
	}
	switch ov := orig.(type) {
	case map[string]interface{}:
		mv, ok := mig.(map[string]interface{})
		if !ok {
			*out = append(*out, prefixOrRoot(prefix))
			*limit--
			return
		}
		keys := mapKeys(ov, mv)
		for _, k := range keys {
			existsInBoth := true
			ovv, okO := ov[k]
			mvv, okM := mv[k]
			if !okO || !okM {
				existsInBoth = false
			}
			child := prefix + "." + k
			if !existsInBoth {
				*out = append(*out, child)
				*limit--
				continue
			}
			if !equalValues(ovv, mvv) {
				collect(ovv, mvv, child, out, limit)
			}
		}
	case []interface{}:
		mv, ok := mig.([]interface{})
		if !ok {
			*out = append(*out, prefixOrRoot(prefix))
			*limit--
			return
		}
		if len(ov) != len(mv) {
			*out = append(*out, fmt.Sprintf("%s[] (length orig=%d mig=%d)", prefixOrRoot(prefix), len(ov), len(mv)))
			*limit--
		}
		n := len(ov)
		if len(mv) < n {
			n = len(mv)
		}
		for i := 0; i < n; i++ {
			if !equalValues(ov[i], mv[i]) {
				collect(ov[i], mv[i], fmt.Sprintf("%s[]", prefixOrRoot(prefix)), out, limit)
			}
		}
	default:
		if !equalValues(orig, mig) {
			*out = append(*out, prefixOrRoot(prefix))
			*limit--
		}
	}
}

func prefixOrRoot(prefix string) string {
	if prefix == "" {
		return "$"
	}
	return prefix
}

func equalValues(a, b interface{}) bool {
	return reflect.DeepEqual(a, b)
}

func mapKeys(a, b map[string]interface{}) []string {
	seen := map[string]bool{}
	var keys []string
	for k := range a {
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	for k := range b {
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	return keys
}

// stripIgnored removes (sentinel-replaces) values at the configured json
// paths so volatile fields like ids, tokens or timestamps do not count as
// discrepancies.
func stripIgnored(v interface{}, ignoreFields []string) interface{} {
	for _, path := range ignoreFields {
		parts := strings.Split(path, ".")
		v = replacePath(v, parts)
	}
	return v
}

func replacePath(node interface{}, parts []string) interface{} {
	if len(parts) == 0 {
		return node
	}
	key := parts[0]
	rest := parts[1:]

	switch val := node.(type) {
	case map[string]interface{}:
		if len(rest) == 0 {
			val[key] = sentinel
		} else if child, ok := val[key]; ok {
			val[key] = replacePath(child, rest)
		} else {
			val[key] = materializePath(node, rest)
		}
	case []interface{}:
		for i := range val {
			if key == "*" {
				val[i] = replacePath(val[i], parts[1:])
			} else if key == fmt.Sprintf("%d", i) {
				val[i] = replacePath(val[i], rest)
			}
		}
	}
	return node
}

// materializePath creates the missing intermediate container so the sentinel
// exists on both sides for comparison purposes.
func materializePath(seed interface{}, parts []string) interface{} {
	container := map[string]interface{}{}
	cur := container
	for i := 0; i < len(parts)-1; i++ {
		next := map[string]interface{}{}
		cur[parts[i]] = next
		cur = next
	}
	cur[parts[len(parts)-1]] = sentinel
	return container[parts[0]]
}

func parseJSON(body []byte) (interface{}, bool) {
	trimmed := trimSpace(body)
	if len(trimmed) == 0 {
		return nil, false
	}
	var v interface{}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	return v, true
}

func preview(body []byte, max int) string {
	if len(body) > max {
		return string(body[:max]) + "…"
	}
	return string(body)
}

func trimSpace(b []byte) []byte {
	return bytes.TrimSpace(b)
}