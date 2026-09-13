# api-migrate-quality-test

A drop-in proxy that validates an API migration by mirroring idempotent
traffic to the **original** service and the **migrated** service in parallel,
comparing every response, and exposing a Markdown quality report with
performance metrics.

## How it works

```
                    ┌──────────────────────────────────────────┐
                    │   api-migrate-quality-test (this proxy)  │
 client ──────────► │                                          │
                    │   idempotent method + allowed route?     │
                    │   ┌─ GET /users/1 (parallel, both) ─┐    │
                    │   │                                  │    │
                    │   ▼                                  ▼    │
                    │  original                     migrated   │
                    │  service  ◄───────────────►    service   │
                    │  (responds to client)                  │
                    │  │                                     │
                    │  └──────────► SQLite (comparison) ◄────┘
                    │        (flushed on every run)            │
                    └───────────────┬──────────────────────────┘
                                    ▼
                        GET /report  →  Markdown quality report
```

- **Idempotent calls** (`GET`, `HEAD`, `OPTIONS`, `PUT`, `DELETE` by default)
  are sent to **both targets in parallel**, compared, and the result stored.
- **Everything else** (`POST`, `PATCH`, …) is routed only to the original
  service — the proxy never alters non-idempotent traffic.
- The caller **always** receives the original service's response, so the proxy
  is safe to put in front of real traffic during validation.
- Comparison covers **HTTP status**, **configured headers**, and the
  **JSON body** (ignoring volatile fields you configure).

## Quick start

```bash
cp config.example.yaml config.yaml
# point `original` and `migrated` at your services
go build -o apimigrate .
./apimigrate -config config.yaml
```

Send traffic through the proxy (default `http://localhost:8080`), then browse
to the report:

- Markdown report: `http://localhost:8080/report`
- JSON report:     `http://localhost:8080/report.json`

## Report

The Markdown report includes:

- **Summary:** total mirrored requests, matched vs discrepancies, match rate,
  per-category mismatch counts and transport errors.
- **Performance:** avg / p50 / p95 / max latency for original vs migrated,
  latency difference, regression flag, and average response sizes — both
  overall and per endpoint.
- **Discrepancies:** every failing request with status codes, latency diff,
  mismatched header names, and the exact **JSON paths** where bodies diverge
  (`data.items[0].price`), plus the **complete original and migrated response
  bodies** side by side when `store_full_bodies` is enabled (default).

## Configuration

See `config.example.yaml` for a fully commented example.

| Key | Default | Description |
|---|---|---|
| `server.host` / `server.port` | `0.0.0.0:8080` | Proxy listen address |
| `original` | – | Original service base URL |
| `migrated` | – | Migrated service base URL |
| `timeout` | `30s` | Upstream client timeout |
| `idempotent_methods` | GET, HEAD, OPTIONS, PUT, DELETE | Methods mirrored & compared |
| `routes.allow` | `[]` (all) | Path patterns to mirror |
| `routes.deny` | `[]` | Path patterns to never mirror (wins over `allow`) |
| `routes.match_mode` | `prefix` | `prefix` (string prefix) or `regex` (Go RE2 expressions) |
| `headers_to_compare` | `[]` (all) | Headers included in comparison |
| `ignore_fields` | `[]` | JSON paths excluded from body comparison (`a.b`, `arr.*`) |
| `body_preview_chars` | `800` | Response body bytes kept for debugging |
| `db.path` | `./migration.db` | SQLite file, flushed on every run |
| `report.endpoint` | `/report` | Markdown report route |
| `report.json_endpoint` | `/report.json` | JSON report route |
| `report.store_bodies` | `false` | Keep truncated body previews in the report |
| `report.store_full_bodies` | `true` | Keep COMPLETE bodies for mismatched records |
| `report.latency_tolerance_ms` | `100` | Per-endpoint regression threshold |

### Route matching

`routes.match_mode` controls how `allow` / `deny` entries are matched:

```yaml
# prefix (default): plain string prefix match
routes:
  match_mode: prefix
  allow: ["/api/users"]
  deny:  ["/api/users/admin"]

# regex: Go (RE2) regular expressions
routes:
  match_mode: regex
  allow: ["^/api/users/[0-9]+$"]
  deny:  ["^/api/users/999$"]
```

The decision per request is: method is idempotent **and** path matches an
`allow` entry **and not** a `deny` entry (empty `allow` = every path allowed).
Deny always wins.

## Why idempotent only?

Mirroring a non-idempotent request (e.g. `POST`) to two services would mutate
state twice and is unsafe. By restricting comparison to idempotent methods the
proxy produces read-only measurements: any no-op consumer can safely point its
GET/DELETE traffic at it while everything else keeps going to the primary.

## Example: full test rig

The `example/` directory ships a ready-made test rig you can run against any
Kubernetes cluster to see the tool working, then tears everything down.

```
example/
├── mockservice/       # tiny mock API; deployed twice (original + migrated)
├── mockservice/main.go# "migrated" replies with minor, intentional diffs
├── k8s/               # namespace, configmap, mock, proxy, traffic manifests
├── config.local.yaml  # config for the local demo (no k8s)
├── demo-local.sh      # local demo: no k8s required, self-cleaning
└── run-test.sh        # k8s end-to-end: deploy -> traffic -> report -> cleanup
```

The mock "migrated" service differs subtly so the report has something to show:

| Endpoint | Difference on migrated |
|---|---|
| `GET /api/users/{id}` | name value differs, `email` renamed to `contact_email`, extra `preferences` |
| `GET /api/orders` | items gain `currency`, `X-Api-Version` header differs, slower (+20 ms) |
| `GET /api/products/{id}` | extra `warranty` field |
| `GET /api/users/missing` | identical 404 (should match) |
| `GET /api/health` | identical, excluded from mirroring |

### Local (no k8s)

```bash
./example/demo-local.sh          # starts mocks + proxy, pumps traffic, prints report, self-cleans
```

### Kubernetes (deploy, test, auto-cleanup)

```bash
./example/run-test.sh             # builds images if needed, deploys, pumps traffic for 30s,
                                  # fetches ./example/report.md, then deletes the namespace
./example/run-test.sh -s 120      # keep traffic flowing longer for bigger sample
./example/run-test.sh -k          # keep the k8s resources around afterwards
./example/run-test.sh -b          # force a rebuild of the images
```

The script only touches the `migration-test` namespace, reuses locally-built
images when present (fast re-runs), guards every network call with retries/timeouts,
and **always deletes the namespace on exit** unless `-k` is passed.

### Example report excerpt

```markdown
## Performance by endpoint
| Endpoint | Count | Match% | Avg orig (ms) | Avg mig (ms) | Diff (ms) | P95 orig (ms) | P95 mig (ms) | Degraded |
| `GET /api/orders` | 18 | 0% | 6.43 | 26.55 | +20.12 | 6.98 | 28.88 | REGRESSED |

### 1. `GET /api/users/1`
- **Body diff paths:** `$.email`, `$.name`, `$.preferences`, `$.contact_email`
**Original response (full):**  {...}
**Migrated response (full):**  {...}
```

## Storage

Results live in a single lightweight SQLite database (`migration.db`) that is
**dropped and recreated each time the process starts**, so it never accumulates
history and is safe to use for long soak runs. Fetch the report at any moment
during the run.