package proxy

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strings"
	"time"

	"sievegate/config"
	"sievegate/report"
	"sievegate/store"
)

// hopByHop headers must not be forwarded end to end.
var hopByHop = []string{
	"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
	"Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

type headerMap map[string][]string

func (h headerMap) get(name string) string {
	if vv, ok := h[http.CanonicalHeaderKey(name)]; ok && len(vv) > 0 {
		return vv[0]
	}
	return ""
}

// compiledRewrite is a precompiled URL rewrite rule. In regex mode rx holds the
// compiled From pattern; in prefix mode only from/to are used.
type compiledRewrite struct {
	from string
	to   string
	rx   *regexp.Regexp
}

type capturedResponse struct {
	label    string
	status   int
	header   headerMap
	body     []byte
	duration time.Duration
	err      error
}

// A Proxy mirrors idempotent requests to both the original and migrated
// service, compares the responses, stores the comparison, and always answers
// the caller with the original service's response.
type Proxy struct {
	cfg       *config.Config
	db        *store.Store
	origURL   *url.URL
	origProxy *httputil.ReverseProxy
	client    *http.Client
	runID     string
	allowRx   []*regexp.Regexp
	denyRx    []*regexp.Regexp
	rewrites  []compiledRewrite
	regexMode bool
}

func New(cfg *config.Config, db *store.Store, runID string) (*Proxy, error) {
	if cfg.Original == "" {
		return nil, errors.New("proxy: original target URL is required")
	}
	origURL, err := url.Parse(cfg.Original)
	if err != nil {
		return nil, fmt.Errorf("proxy: invalid original URL: %w", err)
	}

	p := &Proxy{
		cfg:       cfg,
		db:        db,
		origURL:   origURL,
		origProxy: httputil.NewSingleHostReverseProxy(origURL),
		client:    &http.Client{Timeout: cfg.Timeout.Duration},
		runID:     runID,
	}

	if err := p.compileRoutes(); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	// Report endpoints are served locally and never proxied.
	if p.cfg.Report.Endpoint != "" && r.URL.Path == p.cfg.Report.Endpoint {
		report.Handler(p.cfg, p.db, p.runID, "markdown").ServeHTTP(w, r)
		return
	}
	if p.cfg.Report.JSONEndpoint != "" && r.URL.Path == p.cfg.Report.JSONEndpoint {
		report.Handler(p.cfg, p.db, p.runID, "json").ServeHTTP(w, r)
		return
	}

	switch {
	case p.shouldMirror(r):
		p.handleMirrored(w, r)
		log.Printf("[mirror] %s %s took=%s", r.Method, r.URL.RequestURI(), time.Since(start).Round(time.Microsecond))
	default:
		p.handleForward(w, r)
		log.Printf("[forward] %s %s took=%s", r.Method, r.URL.RequestURI(), time.Since(start).Round(time.Microsecond))
	}
}

// shouldMirror reports whether a request should be sent to both targets and
// compared.
func (p *Proxy) shouldMirror(r *http.Request) bool {
	if !p.cfg.IsIdempotent(r.Method) {
		return false
	}
	return p.matchesRoutes(r.URL.Path)
}

func (p *Proxy) matchesRoutes(path string) bool {
	if p.regexMode {
		for _, deny := range p.denyRx {
			if deny.MatchString(path) {
				return false
			}
		}
		if len(p.allowRx) == 0 {
			return true
		}
		for _, allow := range p.allowRx {
			if allow.MatchString(path) {
				return true
			}
		}
		return false
	}

	for _, deny := range p.cfg.Routes.Deny {
		if strings.HasPrefix(path, deny) {
			return false
		}
	}
	if len(p.cfg.Routes.Allow) == 0 {
		return true
	}
	for _, allow := range p.cfg.Routes.Allow {
		if strings.HasPrefix(path, allow) {
			return true
		}
	}
	return false
}

// compileRoutes precompiles allow/deny/rewrite patterns. Only regex mode
// precompiles regular expressions; prefix mode matches plain strings.
func (p *Proxy) compileRoutes() error {
	for _, rw := range p.cfg.Routes.Rewrite {
		p.rewrites = append(p.rewrites, compiledRewrite{from: rw.From, to: rw.To})
	}
	if p.cfg.Routes.MatchMode != "regex" {
		return nil
	}
	p.regexMode = true
	for _, allow := range p.cfg.Routes.Allow {
		rx, err := regexp.Compile(allow)
		if err != nil {
			return fmt.Errorf("config: routes.allow pattern %q: %w", allow, err)
		}
		p.allowRx = append(p.allowRx, rx)
	}
	for _, deny := range p.cfg.Routes.Deny {
		rx, err := regexp.Compile(deny)
		if err != nil {
			return fmt.Errorf("config: routes.deny pattern %q: %w", deny, err)
		}
		p.denyRx = append(p.denyRx, rx)
	}
	for i := range p.rewrites {
		rx, err := regexp.Compile(p.rewrites[i].from)
		if err != nil {
			return fmt.Errorf("config: routes.rewrite.from %q: %w", p.rewrites[i].from, err)
		}
		p.rewrites[i].rx = rx
	}
	return nil
}

// MatchesRoutesForTest exposes route matching for integration tests.
func (p *Proxy) MatchesRoutesForTest(path string) bool {
	return p.matchesRoutes(path)
}

// rewritePath applies the configured URL rewrites to a request path. Rules are
// applied in order; the first match wins. In regex mode From is a RE2 pattern
// and To may reference capture groups; in prefix mode the From prefix is
// replaced with To.
func (p *Proxy) rewritePath(path string) string {
	for _, rw := range p.rewrites {
		if p.regexMode {
			if rw.rx.MatchString(path) {
				return rw.rx.ReplaceAllString(path, rw.to)
			}
		} else if strings.HasPrefix(path, rw.from) {
			return rw.to + path[len(rw.from):]
		}
	}
	return path
}

// RewritePathForTest exposes path rewriting for integration tests.
func (p *Proxy) RewritePathForTest(path string) string {
	return p.rewritePath(path)
}

func (p *Proxy) handleForward(w http.ResponseWriter, r *http.Request) {
	// Non-idempotent or un-mirrored requests pass straight through to the
	// original service, apart from configured URL rewrites.
	r.URL.Path = p.rewritePath(r.URL.Path)
	p.origProxy.ServeHTTP(w, r)
}

func (p *Proxy) handleMirrored(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to read request body: %v", err), http.StatusBadRequest)
		return
	}

	// Route matching (interception) uses the incoming path, but the path sent
	// upstream is the rewritten one. The comparison is recorded against the
	// original intercepted path.
	upstreamPath := p.rewritePath(r.URL.Path)
	info := requestInfo{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery}

	ch := make(chan capturedResponse, 2)
	go p.capture("original", p.cfg.Original, upstreamPath, r, body, ch)
	go p.capture("migrated", p.cfg.Migrated, upstreamPath, r, body, ch)

	var origRes, migRes capturedResponse
	for i := 0; i < 2; i++ {
		res := <-ch
		switch res.label {
		case "original":
			origRes = res
		case "migrated":
			migRes = res
		}
	}

	comparison := Compare(info, origRes, migRes, p.cfg, p.runID)
	if !p.cfg.Report.StoreBodies {
		// Keep the stored payload minimal unless the operator opts in.
		comparison.BodyPreviewOriginal = ""
		comparison.BodyPreviewMigrated = ""
	}
	if !comparison.IsMatch && p.cfg.Report.StoreFullBodies {
		// Hold the complete response bodies for mismatched records so the
		// report can show the full diff data.
		comparison.BodyOriginalFull = string(origRes.body)
		comparison.BodyMigratedFull = string(migRes.body)
	}
	if err := p.db.Save(comparison); err != nil {
		log.Printf("[store] failed to save comparison: %v", err)
	}

	// Always answer the caller with the original service's response.
	if !comparison.IsMatch {
		w.Header().Set("X-APIMigrate-Matched", "false")
	}
	p.writeResponse(w, origRes)

	if comparison.IsMatch {
		log.Printf("[mirror] MATCH   %s %s orig=%s mig=%s", r.Method, r.URL.RequestURI(),
			comparison.DurationOriginal.Round(time.Microsecond), comparison.DurationMigrated.Round(time.Microsecond))
	} else {
		log.Printf("[mirror] MISMATCH %s %s orig=%s mig=%s diffs=%v", r.Method, r.URL.RequestURI(),
			comparison.DurationOriginal.Round(time.Microsecond), comparison.DurationMigrated.Round(time.Microsecond),
			comparison.DiffPaths)
	}
}

func (p *Proxy) capture(label, target, path string, r *http.Request, body []byte, ch chan<- capturedResponse) {
	req, err := p.buildTargetRequest(target, path, r, body)
	if err != nil {
		ch <- capturedResponse{label: label, err: err}
		return
	}

	start := time.Now()
	resp, err := p.client.Do(req)
	duration := time.Since(start)
	if err != nil {
		ch <- capturedResponse{label: label, err: err, duration: duration}
		return
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		ch <- capturedResponse{label: label, status: resp.StatusCode, header: headerMap(resp.Header), duration: duration, err: err}
		return
	}

	ch <- capturedResponse{
		label:    label,
		status:   resp.StatusCode,
		header:   headerMap(resp.Header.Clone()),
		body:     data,
		duration: duration,
	}
}

func (p *Proxy) buildTargetRequest(target, path string, r *http.Request, body []byte) (*http.Request, error) {
	u, err := url.Parse(target)
	if err != nil {
		return nil, err
	}
	u.Path = path
	u.RawQuery = r.URL.RawQuery

	req, err := http.NewRequest(r.Method, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	// Forward the essential headers; skip hop-by-hop headers.
	for name, values := range r.Header {
		if isHopByHop(name) {
			continue
		}
		for _, v := range values {
			req.Header.Add(name, v)
		}
	}
	return req, nil
}

func (p *Proxy) writeResponse(w http.ResponseWriter, res capturedResponse) {
	if res.err != nil {
		http.Error(w, fmt.Sprintf("original service error: %v", res.err), http.StatusBadGateway)
		return
	}
	for name, values := range res.header {
		if isHopByHop(name) {
			continue
		}
		for _, v := range values {
			w.Header().Add(name, v)
		}
	}
	w.WriteHeader(res.status)
	_, _ = w.Write(res.body)
}

func isHopByHop(name string) bool {
	for _, h := range hopByHop {
		if http.CanonicalHeaderKey(name) == h {
			return true
		}
	}
	return false
}