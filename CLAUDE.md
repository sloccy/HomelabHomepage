# Lantern — Claude Code Guidelines

## Project overview

Lantern is a homelab reverse proxy with automatic service discovery, Cloudflare DNS/tunnel integration, ACME certificate management, and a web UI. It runs as a single long-running Go binary.

## Architecture

- All packages live under `internal/` — nothing is a public API
- Entry point: `main.go`
- Key packages: `proxy`, `web`, `discovery`, `config`, `cf`, `tunnel`, `certs`, `store`, `util`, `sysinfo`, `ddns`
- Frontend: Bootstrap 5.3.3 via CDN, HTMX, server-rendered Go templates
- No database — JSON file store (`internal/store`)

## Go coding standards

These rules are enforced by `golangci-lint` (`.golangci.yml`). Follow them proactively — don't rely on the linter to catch violations after the fact.

### Error handling
- Use `errors.Is()` / `errors.As()` for error comparisons — never `==`
- Use `%w` in `fmt.Errorf` to wrap errors
- Error strings: lowercase, no trailing punctuation
- If `err != nil`, return the error — never silently return `nil` unless intentional (add `//nolint:nilerr` with explanation)

### HTTP
- Use `http.NoBody` instead of `nil` for request bodies on GET requests
- Use `http.MethodGet`, `http.StatusOK`, etc. — never string/int literals
- Use canonical header names (`"X-Forwarded-For"`, not `"x-forwarded-for"`)
- Always close response bodies
- Always pass `context.Context` to outgoing HTTP requests

### Context
- Thread `context.Context` through call chains — don't use `context.Background()` when a context is available
- In HTTP handlers, use `r.Context()` for synchronous work
- For background goroutines that must outlive a request, use `context.Background()` with a timeout and add `//nolint:contextcheck` with explanation

### Type safety
- Always use checked type assertions: `v, ok := x.(T)` — never bare `x.(T)`
- Exception: `sync.Pool` where the type is guaranteed — add `//nolint:forcetypeassert` with explanation

### Style
- Early returns over deep nesting — `if err != nil { return err }` then continue
- No `else` after `return` / `break` / `continue`
- `i++` not `i += 1`
- `id == ""` not `len(id) == 0` for empty string checks
- Combine same-type function params: `name, category func(T) string`
- Name return values when the function returns 2+ values of the same type

### Banned imports
- `crypto/md5`, `crypto/sha1` — use `crypto/sha256` or `crypto/sha512`
- `unsafe` — not needed in this project
- `net/http/cgi`, `net/http/pprof` — not appropriate for production

### nolint directives
- Every `//nolint` must include an explanation: `//nolint:gosec // reason here`

### Dependencies
- Docker SDK: use `github.com/moby/moby/client` and `github.com/moby/moby/api`, **not** `github.com/docker/docker` (deprecated upstream; last release `v28.5.2` has known unpatched CVEs against the archive API)

## Frontend standards

- Bootstrap 5.3.8 vendored locally (embedded in binary) with dark theme — no Tailwind, no DaisyUI
- Prefer in order: Semantic HTML → Bootstrap classes → `style.css` rules → HTMX → Vanilla JS
- Bootstrap utility classes work: `d-flex`, `gap-*`, `mb-*`, etc.

## Commands

- **Build:** `go build ./...`
- **Test:** `go test ./...`
- **Lint:** `golangci-lint run ./...`
- **Vendor frontend:** `npm ci && node scripts/vendor.js`
