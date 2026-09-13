package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a time.Duration that can be unmarshalled from YAML strings
// like "30s" or "2m".
type Duration struct {
	time.Duration
}

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err != nil {
		return err
	}
	dur, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	d.Duration = dur
	return nil
}

func (d Duration) MarshalYAML() (interface{}, error) {
	return d.Duration.String(), nil
}

type Config struct {
	Server      ServerConfig `yaml:"server"`
	Original    string       `yaml:"original"`
	Migrated    string       `yaml:"migrated"`
	Timeout     Duration     `yaml:"timeout"`

	// IdempotentMethods are mirrored to both targets and compared.
	// All other methods are forwarded only to the original service.
	IdempotentMethods []string `yaml:"idempotent_methods"`

	Routes RouteRules `yaml:"routes"`

	// HeadersToCompare limits header comparison to these header names.
	// If empty, all response headers are compared.
	HeadersToCompare []string `yaml:"headers_to_compare"`

	// IgnoreFields lists JSON paths (dot notation, "*" for array elements)
	// excluded from body comparison, e.g. "data.token", "id", "metadata.*".
	IgnoreFields []string `yaml:"ignore_fields"`

	// BodyPreviewChars controls how many characters of each response body are
	// retained in the report database. Larger values use more memory/disk.
	BodyPreviewChars int `yaml:"body_preview_chars"`

	DB     DBConfig     `yaml:"db"`
	Report ReportConfig `yaml:"report"`
}

type ServerConfig struct {
	Host string `yaml:"host"`
	Port int    `yaml:"port"`
}

// RouteRules control which paths are mirrored. Deny always wins over Allow.
// Patterns are matched using MatchMode ("prefix" or "regex").
// An empty Allow list means every path is eligible.
type RouteRules struct {
	Allow []string `yaml:"allow"`
	Deny  []string `yaml:"deny"`

	// Rewrites maps intercepted request paths to the paths sent upstream
	// (original + migrated) and to forwarded requests. Rules apply in order;
	// the first match wins. See RewriteRule for match semantics.
	Rewrite []RewriteRule `yaml:"rewrite"`

	// MatchMode determines how patterns are matched:
	//   "prefix" (default) - plain string prefix match
	//   "regex"            - Go (RE2) regular expressions
	MatchMode string `yaml:"match_mode"`
}

// RewriteRule rewrites a request path before it is sent upstream, using the
// same match_mode as allow/deny.
//   - "prefix": if the path starts with From, the matched prefix is replaced
//     with To (e.g. From "/api/v1", To "/v2": "/api/v1/users" -> "/v2/users").
//   - "regex": From is a Go (RE2) expression and To may use $1, $2, …
//     capture groups (e.g. From "^/api/v1/(.*)$", To "/v2/$1").
type RewriteRule struct {
	From string `yaml:"from"`
	To   string `yaml:"to"`
}

type DBConfig struct {
	Path string `yaml:"path"`
}

type ReportConfig struct {
	// Endpoint exposes the markdown report, e.g. "/report".
	Endpoint string `yaml:"endpoint"`

	// JSONEndpoint optionally exposes the same report as JSON.
	JSONEndpoint string `yaml:"json_endpoint"`

	// StoreBodies persists response previews in the database for
	// debugging discrepancies.
	StoreBodies bool `yaml:"store_bodies"`

	// StoreFullBodies persists the complete original and migrated response
	// bodies for every record with a discrepancy, so the report can show the
	// full payloads side by side. Disable to keep the database small.
	StoreFullBodies bool `yaml:"store_full_bodies"`

	// LatencyToleranceMs flags a performance regression in the report when
	// the migrated service is slower than the original by this many ms.
	LatencyToleranceMs int `yaml:"latency_tolerance_ms"`
}

// Defaults returns a Config populated with sane defaults.
func Defaults() *Config {
	return &Config{
		Server: ServerConfig{Host: "0.0.0.0", Port: 8080},
		Timeout: Duration{Duration: 30 * time.Second},
		IdempotentMethods: []string{"GET", "HEAD", "OPTIONS", "PUT", "DELETE"},
		BodyPreviewChars: 800,
		DB: DBConfig{Path: "./migration.db"},
		Report: ReportConfig{
			Endpoint:           "/report",
			JSONEndpoint:       "/report.json",
			StoreFullBodies:    true,
			LatencyToleranceMs: 100,
		},
	}
}

// Load reads, validates, and returns a merged config from a YAML file.
func Load(path string) (*Config, error) {
	cfg := Defaults()

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}
	if err := yaml.Unmarshal(raw, cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) Validate() error {
	if c.Original == "" {
		return fmt.Errorf("config: original target URL is required")
	}
	if c.Migrated == "" {
		return fmt.Errorf("config: migrated target URL is required")
	}
	if c.Server.Port == 0 {
		c.Server.Port = 8080
	}
	if c.Timeout.Duration <= 0 {
		c.Timeout.Duration = 30 * time.Second
	}
	if len(c.IdempotentMethods) == 0 {
		c.IdempotentMethods = Defaults().IdempotentMethods
	}
	if c.DB.Path == "" {
		c.DB.Path = "./migration.db"
	}
	if c.BodyPreviewChars == 0 {
		c.BodyPreviewChars = 800
	}
	if c.Report.LatencyToleranceMs == 0 {
		c.Report.LatencyToleranceMs = 100
	}
	switch c.Routes.MatchMode {
	case "":
		c.Routes.MatchMode = "prefix"
	case "prefix", "regex":
	default:
		return fmt.Errorf("config: routes.match_mode must be \"prefix\" or \"regex\", got %q", c.Routes.MatchMode)
	}
	for i, rw := range c.Routes.Rewrite {
		if rw.From == "" {
			return fmt.Errorf("config: routes.rewrite[%d].from is required", i)
		}
	}
	return nil
}

// IsIdempotent reports whether a method is eligible for mirrored comparison.
func (c *Config) IsIdempotent(method string) bool {
	for _, m := range c.IdempotentMethods {
		if m == method {
			return true
		}
	}
	return false
}