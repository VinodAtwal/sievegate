package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"apimigrate/config"
	"apimigrate/models"
	"apimigrate/proxy"
	"apimigrate/store"
)

// mockService is a tiny in-test upstream. The "migrated" variant introduces
// intentional discrepancies and latency so tests can observe them.
func mockService(migrated bool) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/users", func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("id")

		if migrated {
			sleep := 20 * time.Millisecond
			if id == "slow" {
				sleep = 250 * time.Millisecond
			}
			time.Sleep(sleep)
		} else {
			time.Sleep(5 * time.Millisecond)
		}

		if id == "missing" {
			http.NotFound(w, nil)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Api-Version", "v2")

		volatile := time.Now().UnixNano()
		body := fmt.Sprintf(`{"user":{"id":"%s","name":"Jane","email":"jane@x.io"},"server_time":%d}`, id, volatile)

		if migrated && id == "diff" {
			body = fmt.Sprintf(`{"user":{"id":"%s","name":"Janet","email":"jane@x.io"},"server_time":%d}`, id, volatile)
		}
		if migrated && id == "missing-field" {
			body = fmt.Sprintf(`{"user":{"id":"%s","name":"Jane"},"server_time":%d}`, id, volatile)
		}
		io.WriteString(w, body)
	})

	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		if migrated {
			w.WriteHeader(http.StatusInternalServerError)
			io.WriteString(w, `{"error":"boom"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"status":"ok"}`)
	})

	mux.HandleFunc("GET /api/delay", func(w http.ResponseWriter, r *http.Request) {
		if migrated {
			time.Sleep(150 * time.Millisecond)
		} else {
			time.Sleep(10 * time.Millisecond)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	})

	mux.HandleFunc("POST /api/orders", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"order_id":"1234"}`)
	})

	mux.HandleFunc("GET /other", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "outside scope")
	})

	mux.HandleFunc("GET /api/admin", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "admin")
	})

	return httptest.NewServer(mux)
}

func setupProxy(t *testing.T, orig, mig *httptest.Server) (*httptest.Server, *store.Store, *config.Config) {
	t.Helper()
	tmp := t.TempDir()

	cfg := config.Defaults()
	cfg.Original = orig.URL
	cfg.Migrated = mig.URL
	cfg.Server.Port = 0
	cfg.Timeout.Duration = 2 * time.Second
	cfg.Routes.Allow = []string{"/api"}
	cfg.Routes.Deny = []string{"/api/admin"}
	cfg.IgnoreFields = []string{"server_time"}
	cfg.HeadersToCompare = []string{"content-type", "x-api-version"}
	cfg.BodyPreviewChars = 300
	cfg.Report.StoreBodies = true
	cfg.Report.LatencyToleranceMs = 80
	cfg.DB.Path = filepath.Join(tmp, "test.db")

	db, err := store.New(cfg.DB.Path)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	p, err := proxy.New(cfg, db, "test-run-1")
	if err != nil {
		t.Fatalf("proxy: %v", err)
	}

	ts := httptest.NewServer(p)
	t.Cleanup(ts.Close)
	return ts, db, cfg
}

func do(t *testing.T, ts *httptest.Server, method, path string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(method, ts.URL+path, nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res, string(b)
}

func TestEndToEnd(t *testing.T) {
	orig := mockService(false)
	mig := mockService(true)
	defer orig.Close()
	defer mig.Close()

	ts, db, cfg := setupProxy(t, orig, mig)

	// 1. Matching request (volatile field ignored -> should match).
	res, body := do(t, ts, "GET", "/api/users?id=1")
	if res.StatusCode != 200 {
		t.Fatalf("expected 200 from original, got %d", res.StatusCode)
	}
	if !strings.Contains(body, "Jane") {
		t.Fatalf("unexpected body: %s", body)
	}

	// 2. Discrepancy in a field value.
	do(t, ts, "GET", "/api/users?id=diff")

	// 3. Missing field in migrated body.
	do(t, ts, "GET", "/api/users?id=missing-field")

	// 4. Status mismatch.
	do(t, ts, "GET", "/api/status")

	// 5. Slow migrated response (perf regression).
	do(t, ts, "GET", "/api/delay")

	// 6. Not-found on both -> matches.
	do(t, ts, "GET", "/api/users?id=missing")

	// 7. Non-idempotent POST -> forwarded only, never stored/compared.
	res, _ = do(t, ts, "POST", "/api/orders")
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 from POST forward, got %d", res.StatusCode)
	}

	// 8. Route outside scope -> forwarded, not mirrored.
	res, body = do(t, ts, "GET", "/other")
	if res.StatusCode != 200 || body != "outside scope" {
		t.Fatalf("unexpected forwarded response: %d %q", res.StatusCode, body)
	}

	// 9. Denied route -> forwarded only (admin handler returns 200).
	res, body = do(t, ts, "GET", "/api/admin")
	if res.StatusCode != 200 || body != "admin" {
		t.Fatalf("denied route should be forwarded: %d %q", res.StatusCode, body)
	}

	records, err := db.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(records) != 6 {
		t.Fatalf("expected 6 mirrored records, got %d", len(records))
	}

	// Verify expectations per request.
	byPath := map[string]*models.Comparison{}
	for _, rec := range records {
		byPath[rec.Path+"?"+rec.Query] = rec
	}
	key := func(p, q string) string { return p + "?" + q }

	// 1 -> match (ignored volatile field)
	if rec := byPath[key("/api/users", "id=1")]; rec == nil || !rec.IsMatch {
		t.Errorf("id=1 should be a full match, got %+v", rec)
	}
	// diff -> discrepancy on a field value
	diffRec := byPath[key("/api/users", "id=diff")]
	if diffRec == nil || diffRec.IsMatch {
		t.Errorf("id=diff should flag a discrepancy, got %+v", diffRec)
	}
	if diffRec != nil && !containsDiffPath(diffRec.DiffPaths, "$.user.name") {
		t.Errorf("id=diff should report $.user.name diff, got %v", diffRec.DiffPaths)
	}
	if diffRec == nil || diffRec.BodyOriginalFull == "" || diffRec.BodyMigratedFull == "" {
		t.Errorf("id=diff should store full bodies for the diff, orig=%q mig=%q",
			bodyOf(diffRec, true), bodyOf(diffRec, false))
	}
	// missing-field -> discrepancy (missing email)
	if rec := byPath[key("/api/users", "id=missing-field")]; rec == nil || rec.IsMatch {
		t.Errorf("id=missing-field should flag a discrepancy, got %+v", rec)
	}
	// status -> status mismatch
	if rec := byPath[key("/api/status", "")]; rec == nil || rec.IsMatch {
		t.Errorf("GET /api/status should flag a status mismatch, got %+v", rec)
	}
	// missing (same 404 on both) -> match
	if rec := byPath[key("/api/users", "id=missing")]; rec == nil || !rec.IsMatch {
		t.Errorf("id=missing should be a full match (both 404), got %+v", rec)
	}

	// Perf: at least one record where migrated is slower than original.
	var slow bool
	for _, rec := range records {
		if rec.DurationMigrated > rec.DurationOriginal {
			slow = true
		}
	}
	if !slow {
		t.Fatalf("expected at least one record where migrated latency > original")
	}

	// ---- report (markdown) ----
	res, md := do(t, ts, "GET", "/report")
	if res.StatusCode != 200 {
		t.Fatalf("report status: %d", res.StatusCode)
	}
	for _, want := range []string{
		"# API Migration Quality Report",
		"## Summary",
		"## Performance",
		"### Performance by endpoint",
		"## Discrepancies",
		"Match rate",
		"Avg latency",
		"P95 latency",
		"Degraded",
		"id=",
		"$.user.name",
		"**Original response (full):**",
		"**Migrated response (full):**",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("report missing %q", want)
		}
	}

	// ---- report (json) ----
	res, raw := do(t, ts, "GET", "/report.json")
	if res.StatusCode != 200 {
		t.Fatalf("json report status: %d", res.StatusCode)
	}
	var jr struct {
		RunID string `json:"run_id"`
		Total int    `json:"total"`
		Perf  map[string]any
	}
	if err := json.Unmarshal([]byte(raw), &jr); err != nil {
		t.Fatalf("json decode: %v", err)
	}
	if jr.RunID != "test-run-1" {
		t.Errorf("run_id mismatch: %s", jr.RunID)
	}
	if jr.Total != 6 {
		t.Errorf("expected 6 in json report, got %d", jr.Total)
	}

	// ---- ensure original response is returned even on mismatch ----
	res, body = do(t, ts, "GET", "/api/status")
	if res.StatusCode != 200 || !strings.Contains(body, "ok") {
		t.Errorf("client must receive the original (200 ok) response, got %d %q", res.StatusCode, body)
	}
	if res.Header.Get("X-APIMigrate-Matched") != "false" {
		t.Errorf("diagnostic header should mark the mismatch, got %q", res.Header.Get("X-APIMigrate-Matched"))
	}

	// ---- flush-on-restart semantics ----
	// Save some records, close store "simulating restart", then reopen: data gone.
	path := cfg.DB.Path
	first, err := store.New(path)
	if err != nil {
		t.Fatalf("store#1: %v", err)
	}
	first.Save(&models.Comparison{
		RunID: "x", Method: "GET", Path: "/p", IsMatch: true,
		CreatedAt: time.Now(),
	})
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	second, err := store.New(path)
	if err != nil {
		t.Fatalf("store#2 (restart): %v", err)
	}
	defer second.Close()
	recs, err := second.List()
	if err != nil {
		t.Fatalf("list after restart: %v", err)
	}
	if len(recs) != 0 {
		t.Fatalf("expected DB flushed on restart, got %d records", len(recs))
	}
}

func containsDiffPath(paths []string, want string) bool {
	for _, p := range paths {
		if p == want {
			return true
		}
	}
	return false
}

func bodyOf(rec *models.Comparison, original bool) string {
	if rec == nil {
		return "<nil>"
	}
	if original {
		return rec.BodyOriginalFull
	}
	return rec.BodyMigratedFull
}

// TestRegexRouteMatching verifies match_mode: regex scopes mirroring.
func TestRegexRouteMatching(t *testing.T) {
	orig := mockService(false)
	mig := mockService(true)
	defer orig.Close()
	defer mig.Close()

	cfg := config.Defaults()
	cfg.Original = orig.URL
	cfg.Migrated = mig.URL
	cfg.Timeout.Duration = 2 * time.Second
	cfg.Routes.MatchMode = "regex"
	cfg.Routes.Allow = []string{`^/api/users/[0-9]+$`}
	cfg.Routes.Deny = []string{`^/api/users/999$`}
	cfg.DB.Path = filepath.Join(t.TempDir(), "regex.db")

	db, err := store.New(cfg.DB.Path)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	p, err := proxy.New(cfg, db, "regex-run")
	if err != nil {
		t.Fatalf("proxy: %v", err)
	}

	// Route matcher unit checks.
	cases := []struct {
		path string
		want bool
	}{
		{"/api/users/123", true},
		{"/api/users/999", false}, // denied
		{"/api/users/abc", false}, // not numeric
		{"/api/orders/123", false},
		{"/bogus", false},
	}
	for _, tc := range cases {
		got := p.MatchesRoutesForTest(tc.path)
		if got != tc.want {
			t.Errorf("matchesRoutes(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}

	// Invalid regex must fail fast at startup, not silently.
	bad := config.Defaults()
	bad.Original = orig.URL
	bad.Migrated = mig.URL
	bad.Routes.MatchMode = "regex"
	bad.Routes.Allow = []string{"(unclosed"}
	bad.DB.Path = filepath.Join(t.TempDir(), "none.db")
	bdb, err := store.New(bad.DB.Path)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer bdb.Close()
	if _, err := proxy.New(bad, bdb, "bad-run"); err == nil {
		t.Fatalf("expected invalid regex to fail at startup")
	}
}