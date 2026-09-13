package report

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"apimigrate/config"
	"apimigrate/models"
	"apimigrate/store"
)

// Handler renders a report for the current run.
//
//   - section "markdown" produces a human-readable Markdown report with
//     comparison summaries and performance metrics per endpoint.
//   - section "json" produces a machine-readable version.
func Handler(cfg *config.Config, db *store.Store, runID string, section string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		records, err := db.List()
		if err != nil {
			http.Error(w, fmt.Sprintf("failed to read records: %v", err), http.StatusInternalServerError)
			return
		}

		switch section {
		case "json":
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			if err := json.NewEncoder(w).Encode(buildJSON(runID, records, cfg)); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
			}
		default:
			w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
			_, _ = w.Write([]byte(buildMarkdown(runID, records, cfg)))
		}
	}
}

// ---------- statistics ----------

type stats struct {
	records          []*models.Comparison
	total            int
	matched          int
	discrepancy      int
	originalErrors   int
	migratedErrors   int
	origDur          []time.Duration
	migDur           []time.Duration
	origSizes        int64
	migSizes         int64
	statusMismatches int
	headerMismatches int
	bodyMismatches   int
}

func collect(records []*models.Comparison) *stats {
	s := &stats{records: records, total: len(records)}
	for _, rec := range records {
		if rec.IsMatch {
			s.matched++
		} else {
			s.discrepancy++
		}
		if rec.ErrorOriginal != "" {
			s.originalErrors++
		}
		if rec.ErrorMigrated != "" {
			s.migratedErrors++
		}
		if !rec.StatusMatch {
			s.statusMismatches++
		}
		if !rec.HeaderMatch {
			s.headerMismatches++
		}
		if !rec.BodyMatch {
			s.bodyMismatches++
		}
		if rec.DurationOriginal > 0 {
			s.origDur = append(s.origDur, rec.DurationOriginal)
		}
		if rec.DurationMigrated > 0 {
			s.migDur = append(s.migDur, rec.DurationMigrated)
		}
		s.origSizes += int64(rec.SizeOriginal)
		s.migSizes += int64(rec.SizeMigrated)
	}
	return s
}

func summarize(durations []time.Duration) models.LatencySummary {
	sum := models.LatencySummary{Count: len(durations)}
	if len(durations) == 0 {
		return sum
	}
	sorted := make([]time.Duration, len(durations))
	copy(sorted, durations)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	var total time.Duration
	for _, d := range sorted {
		total += d
	}
	sum.Avg = total / time.Duration(len(sorted))
	sum.Median = sorted[len(sorted)/2]
	p95Idx := int(float64(len(sorted))*0.95) - 1
	if p95Idx < 0 {
		p95Idx = 0
	}
	if p95Idx >= len(sorted) {
		p95Idx = len(sorted) - 1
	}
	sum.P95 = sorted[p95Idx]
	sum.Max = sorted[len(sorted)-1]
	return sum
}

// groupByEndpoint groups records by method + path (query string ignored).
func groupByEndpoint(records []*models.Comparison) [][]*models.Comparison {
	groups := map[string][]*models.Comparison{}
	var order []string
	for _, rec := range records {
		key := rec.Method + " " + rec.Path
		if _, ok := groups[key]; !ok {
			order = append(order, key)
		}
		groups[key] = append(groups[key], rec)
	}
	var out [][]*models.Comparison
	for _, key := range order {
		out = append(out, groups[key])
	}
	return out
}

// ---------- markdown ----------

func buildMarkdown(runID string, records []*models.Comparison, cfg *config.Config) string {
	s := collect(records)
	var b strings.Builder

	now := time.Now().UTC().Format(time.RFC3339)
	b.WriteString("# API Migration Quality Report\n\n")
	b.WriteString(fmt.Sprintf("**Run ID:** `%s`  \n", runID))
	b.WriteString(fmt.Sprintf("**Generated:** %s  \n", now))
	b.WriteString(fmt.Sprintf("**Original target:** `%s`  \n", cfg.Original))
	b.WriteString(fmt.Sprintf("**Migrated target:** `%s`  \n", cfg.Migrated))
	b.WriteString(fmt.Sprintf("**Mirrored methods:** `%s`  \n", strings.Join(cfg.IdempotentMethods, ", ")))
	b.WriteString("\n---\n\n")

	b.WriteString("## Summary\n\n")
	if s.total == 0 {
		b.WriteString("No mirrored requests recorded yet. Send some idempotent traffic through the proxy first.\n\n")
		return b.String()
	}

	matchRate := float64(s.matched) / float64(s.total) * 100
	b.WriteString("| Metric | Value |\n")
	b.WriteString("|---|---:|\n")
	b.WriteString(fmt.Sprintf("| Total mirrored requests | %d |\n", s.total))
	b.WriteString(fmt.Sprintf("| Fully matched | %d |\n", s.matched))
	b.WriteString(fmt.Sprintf("| Discrepancies | %d |\n", s.discrepancy))
	b.WriteString(fmt.Sprintf("| Match rate | %.1f%% |\n", matchRate))
	b.WriteString(fmt.Sprintf("| Status-code mismatches | %d |\n", s.statusMismatches))
	b.WriteString(fmt.Sprintf("| Header mismatches | %d |\n", s.headerMismatches))
	b.WriteString(fmt.Sprintf("| Body mismatches | %d |\n", s.bodyMismatches))
	b.WriteString(fmt.Sprintf("| Original transport errors | %d |\n", s.originalErrors))
	b.WriteString(fmt.Sprintf("| Migrated transport errors | %d |\n", s.migratedErrors))
	b.WriteString("\n")

	b.WriteString("## Performance\n\n")
	origLS := summarize(s.origDur)
	migLS := summarize(s.migDur)
	b.WriteString("| Metric | Original | Migrated | Difference |\n")
	b.WriteString("|---|---:|---:|---:|\n")
	b.WriteString(fmt.Sprintf("| Requests sampled | %d | %d | — |\n", origLS.Count, migLS.Count))
	b.WriteString(fmt.Sprintf("| Avg latency | %.2f ms | %.2f ms | %+.2f ms |\n",
		ms(origLS.Avg), ms(migLS.Avg), ms(migLS.Avg)-ms(origLS.Avg)))
	b.WriteString(fmt.Sprintf("| Median (p50) latency | %.2f ms | %.2f ms | %+.2f ms |\n",
		ms(origLS.Median), ms(migLS.Median), ms(migLS.Median)-ms(origLS.Median)))
	b.WriteString(fmt.Sprintf("| P95 latency | %.2f ms | %.2f ms | %+.2f ms |\n",
		ms(origLS.P95), ms(migLS.P95), ms(migLS.P95)-ms(origLS.P95)))
	b.WriteString(fmt.Sprintf("| Max latency | %.2f ms | %.2f ms | %+.2f ms |\n",
		ms(origLS.Max), ms(migLS.Max), ms(migLS.Max)-ms(origLS.Max)))
	b.WriteString(fmt.Sprintf("| Avg response size | %s | %s | %s |\n",
		humanSize(s.origSizes, s.total), humanSize(s.migSizes, s.total),
		humanSize(s.migSizes-s.origSizes, s.total)))
	b.WriteString("\n")

	b.WriteString("### Performance by endpoint\n\n")
	b.WriteString("| Endpoint | Count | Match% | Avg orig (ms) | Avg mig (ms) | Diff (ms) | P95 orig (ms) | P95 mig (ms) | Degraded |\n")
	b.WriteString("|---|---|---:|---:|---:|---:|---:|---:|:---:|\n")
	for _, group := range groupByEndpoint(records) {
		gs := collect(group)
		gorig := summarize(gs.origDur)
		gmig := summarize(gs.migDur)
		diffMS := ms(gmig.Avg) - ms(gorig.Avg)
		degraded := diffMS > float64(cfg.Report.LatencyToleranceMs) && (ms(gorig.Avg) > 1 || diffMS > 0)
		degradedMark := ""
		if degraded {
			degradedMark = "REGRESSED"
		}
		matchPct := "—"
		if len(group) > 0 {
			matched := 0
			for _, rec := range group {
				if rec.IsMatch {
					matched++
				}
			}
			matchPct = fmt.Sprintf("%.0f%%", float64(matched)/float64(len(group))*100)
		}
		b.WriteString(fmt.Sprintf("| `%s %s` | %d | %s | %.2f | %.2f | %+.2f | %.2f | %.2f | %s |\n",
			group[0].Method, group[0].Path, len(group), matchPct,
			ms(gorig.Avg), ms(gmig.Avg), diffMS, ms(gorig.P95), ms(gmig.P95), degradedMark))
	}
	b.WriteString("\n")

	b.WriteString("## Discrepancies\n\n")
	if s.discrepancy == 0 && s.originalErrors == 0 && s.migratedErrors == 0 {
		b.WriteString("No discrepancies found. 💚\n\n")
		return b.String()
	}

	for i, rec := range records {
		if rec.IsMatch {
			continue
		}
		b.WriteString(fmt.Sprintf("### %d. `%s %s`\n\n", i+1, rec.Method, displayPath(rec)))
		b.WriteString(fmt.Sprintf("- **Time:** %s\n", rec.CreatedAt.UTC().Format(time.RFC3339)))
		b.WriteString(fmt.Sprintf("- **Original:** HTTP %d · %s · %s\n",
			rec.StatusOriginal, msDur(rec.DurationOriginal), humanSize(int64(rec.SizeOriginal), 1)))
		b.WriteString(fmt.Sprintf("- **Migrated:** HTTP %d · %s · %s\n",
			rec.StatusMigrated, msDur(rec.DurationMigrated), humanSize(int64(rec.SizeMigrated), 1)))

		latencyDiff := ms(rec.DurationMigrated) - ms(rec.DurationOriginal)
		b.WriteString(fmt.Sprintf("- **Latency diff (mig − orig):** %+.2f ms\n\n", latencyDiff))

		switch {
		case rec.ErrorOriginal != "":
			b.WriteString(fmt.Sprintf("- **Original error:** `%s`\n\n", rec.ErrorOriginal))
		}

		switch {
		case rec.ErrorMigrated != "":
			b.WriteString(fmt.Sprintf("- **Migrated error:** `%s`\n\n", rec.ErrorMigrated))
		}

		if !rec.StatusMatch {
			b.WriteString(fmt.Sprintf("- **Status mismatch:** `%d` vs `%d`\n\n", rec.StatusOriginal, rec.StatusMigrated))
		}
		if !rec.HeaderMatch {
			b.WriteString("- **Header mismatches:**\n")
			for _, hm := range rec.HeaderMismatches {
				b.WriteString(fmt.Sprintf("  - `%s`\n", hm))
			}
			b.WriteString("\n")
		}
		if !rec.BodyMatch {
			b.WriteString("- **Body diff paths:**\n")
			for _, dp := range rec.DiffPaths {
				b.WriteString(fmt.Sprintf("  - `%s`\n", dp))
			}
			b.WriteString("\n")
			if rec.BodyOriginalFull != "" || rec.BodyMigratedFull != "" {
				b.WriteString("**Original response (full):**\n\n```json\n" + rec.BodyOriginalFull + "\n```\n\n")
				b.WriteString("**Migrated response (full):**\n\n```json\n" + rec.BodyMigratedFull + "\n```\n\n")
			} else if cfg.Report.StoreBodies {
				b.WriteString("**Original body preview:**\n\n```json\n" + rec.BodyPreviewOriginal + "\n```\n\n")
				b.WriteString("**Migrated body preview:**\n\n```json\n" + rec.BodyPreviewMigrated + "\n```\n\n")
			}
		}
		b.WriteString("---\n\n")
	}

	return b.String()
}

func displayPath(rec *models.Comparison) string {
	if rec.Query != "" {
		return rec.Path + "?" + rec.Query
	}
	return rec.Path
}

// ---------- json ----------

type jsonReport struct {
	RunID      string       `json:"run_id"`
	Generated  string       `json:"generated"`
	Original   string       `json:"original_target"`
	Migrated   string       `json:"migrated_target"`
	Total      int          `json:"total"`
	Matched    int          `json:"matched"`
	MatchRate  float64      `json:"match_rate"`
	Perf       map[string]any `json:"performance"`
	Discreps   []jsonRecord  `json:"discrepancies"`
}

type jsonRecord struct {
	Method        string   `json:"method"`
	Path          string   `json:"path"`
	Query         string   `json:"query,omitempty"`
	StatusOrig    int      `json:"status_original"`
	StatusMig     int      `json:"status_migrated"`
	StatusMatch   bool     `json:"status_match"`
	HeaderMatch   bool     `json:"header_match"`
	BodyMatch     bool     `json:"body_match"`
	IsMatch       bool     `json:"is_match"`
	DurationOrigMS float64 `json:"duration_original_ms"`
	DurationMigMS float64  `json:"duration_migrated_ms"`
	LatencyDiffMS  float64 `json:"latency_diff_ms"`
	SizeOrig      int      `json:"size_original"`
	SizeMig       int      `json:"size_migrated"`
	ErrorOrig     string  `json:"error_original,omitempty"`
	ErrorMig      string  `json:"error_migrated,omitempty"`
	FullBodyOrig  string  `json:"body_original,omitempty"`
	FullBodyMig   string  `json:"body_migrated,omitempty"`
	DiffPaths     []string `json:"diff_paths,omitempty"`
}

func buildJSON(runID string, records []*models.Comparison, cfg *config.Config) jsonReport {
	s := collect(records)
	origLS := summarize(s.origDur)
	migLS := summarize(s.migDur)

	matchRate := 0.0
	if s.total > 0 {
		matchRate = float64(s.matched) / float64(s.total) * 100
	}

	perf := map[string]any{
		"orig": map[string]any{
			"count":  origLS.Count,
			"avg_ms": round2(ms(origLS.Avg)),
			"p50_ms": round2(ms(origLS.Median)),
			"p95_ms": round2(ms(origLS.P95)),
			"max_ms": round2(ms(origLS.Max)),
		},
		"migrated": map[string]any{
			"count":  migLS.Count,
			"avg_ms": round2(ms(migLS.Avg)),
			"p50_ms": round2(ms(migLS.Median)),
			"p95_ms": round2(ms(migLS.P95)),
			"max_ms": round2(ms(migLS.Max)),
		},
		"latency_diff_avg_ms": round2(ms(migLS.Avg) - ms(origLS.Avg)),
		"tolerance_ms":        cfg.Report.LatencyToleranceMs,
	}

	var disc []jsonRecord
	for _, rec := range records {
		jr := jsonRecord{
			Method: rec.Method, Path: rec.Path, Query: rec.Query,
			StatusOrig: rec.StatusOriginal, StatusMig: rec.StatusMigrated,
			StatusMatch: rec.StatusMatch, HeaderMatch: rec.HeaderMatch,
			BodyMatch: rec.BodyMatch, IsMatch: rec.IsMatch,
			DurationOrigMS: round2(ms(rec.DurationOriginal)),
			DurationMigMS:  round2(ms(rec.DurationMigrated)),
			LatencyDiffMS:  round2(ms(rec.DurationMigrated) - ms(rec.DurationOriginal)),
			SizeOrig:       rec.SizeOriginal, SizeMig: rec.SizeMigrated,
			ErrorOrig: rectoNull(rec.ErrorOriginal), ErrorMig: rectoNull(rec.ErrorMigrated),
			FullBodyOrig: rec.BodyOriginalFull, FullBodyMig: rec.BodyMigratedFull,
			DiffPaths: rec.DiffPaths,
		}
		if jr.IsMatch {
			continue
		}
		disc = append(disc, jr)
	}

	return jsonReport{
		RunID: runID, Generated: time.Now().UTC().Format(time.RFC3339),
		Original: cfg.Original, Migrated: cfg.Migrated,
		Total: s.total, Matched: s.matched, MatchRate: round2(matchRate),
		Perf: perf, Discreps: disc,
	}
}

// ---------- helpers ----------

func ms(d time.Duration) float64 {
	return float64(d) / float64(time.Millisecond)
}

func msDur(d time.Duration) string {
	if d == 0 {
		return "0 ms"
	}
	return fmt.Sprintf("%.2f ms", ms(d))
}

func round2(v float64) float64 {
	return float64(int64(v*100)) / 100
}

func humanSize(bytes int64, count int) string {
	if count <= 0 {
		count = 1
	}
	avg := float64(bytes) / float64(count)
	switch {
	case avg >= 1024*1024:
		return fmt.Sprintf("%.1f MB", avg/(1024*1024))
	case avg >= 1024:
		return fmt.Sprintf("%.1f KB", avg/1024)
	default:
		return fmt.Sprintf("%.0f B", avg)
	}
}

func rectoNull(s string) string {
	if s == "" {
		return ""
	}
	return s
}