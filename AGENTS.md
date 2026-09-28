# AGENTS.md

Lolly is a high-performance HTTP server / reverse proxy (Go, built on fasthttp,
nginx-style YAML config). Module path is `rua.plus/lolly` — **not** a
`github.com/...` path, don't guess it. Requires Go 1.26+ (see `go.mod`).

## Build & run

- `make build` — static binary (`CGO_ENABLED=0`) to `bin/lolly`.
- `make run` — `go run main.go -c lolly.yaml`.
- `./bin/lolly -g -o config.yaml` generates a default config; `-i nginx.conf -o lolly.yaml` imports an nginx config.
- `main.go` is a thin flag-parsing shim; real startup/lifecycle logic is in `internal/app`.

## Testing — three tiers, gated by build tags

- `make test` — unit tests, `go test ./internal/...`, no build tag.
- `make test-integration` — build tag `integration`, in-process (`internal/integration`).
- `make test-e2e` — build tag `e2e`, spins up real containers via testcontainers-go
  (`internal/e2e`) — **requires a running Docker daemon**.
  - `make test-e2e-short` runs only `internal/e2e/testutil` in `-short` mode; no Docker needed.
- `make test-all` runs all three tiers in parallel.
- Test files are guarded by `//go:build integration` / `//go:build e2e`; if you
  run `go test` on them without the matching `-tags`, they silently don't compile in
  (not a failure, just missing).
- `make test-config` does **not** validate a config you point it at — it just
  generates the default template to `/tmp` and checks that succeeds.

## Quality gate — order matters

`make check` = `fmt` → `lint` → `test-all`. Do the same manually.

- `fmt` runs **gofumpt** (stricter than `gofmt`/`go fmt`), not plain `go fmt`.
  Install: `go install mvdan.cc/gofumpt@latest`.
- `lint` runs `golangci-lint` v2 (`.golangci.yml`), `default: all` with ~50
  linters explicitly disabled (`gosec`, `mnd`, `wrapcheck`, `dupl`, `revive`
  stutter check, `errcheck` on tests, etc.). These are intentional project
  exemptions — don't "fix" things those linters would otherwise flag.

## Comment convention — read `docs/comments.md`

All comments in this codebase are **Chinese**, following a specific format:
- every file has a package/file header comment (purpose, notes, `作者：xfy`);
- every exported *and* unexported function gets a doc comment (参数/返回值 for
  non-trivial ones);
- comments explain *why*, not *what*.

This is a project convention only (not enforced by lint — comment-style
linters like `godot`/`godoclint` are disabled). Match it when touching Go files.

## Code layout

- `internal/` holds the actual application, split by concern: `app` (entry/signals),
  `config`, `server`, `handler` (static files), `proxy`, `loadbalance`, `matcher`
  (location matching), `middleware/*` (compression/security/rewrite/accesslog/bodylimit/errorintercept),
  `lua` (nginx-lua-compatible scripting), `http2`/`http3`/`adapter`, `stream` (L4 proxy),
  `ssl`, `cache`, `resolver` (DNS), `variable`, `logging`, `converter` (nginx→lolly config).
- `gjson/` at repo root is **first-party** code (a lua-cjson-compatible JSON
  encoder used by the Lua engine, imported as `rua.plus/lolly/gjson`) — it's
  not a vendored third-party copy, and it's written with English comments
  (unlike the Chinese convention used everywhere else).

## Benchmarks

- `make bench` / `make bench-stat` (10x samples), then `make bench-compare`
  (needs `benchstat`) against `benchmark-baseline.txt` and the thresholds in
  `.benchmark-thresholds.yaml`. Save a new baseline with `make bench-save`.

## PGO builds

`make build-pgo` needs a profile first: enable `monitoring.pprof` in a config,
run the binary under real load, `curl .../debug/pprof/profile?seconds=30 > default.pgo`,
then build. `make pgo-collect` prints this whole recipe.

## Commits

- 每完成一个功能点就提交一次，不要把多个功能点堆到一个大提交里。
- 提交信息遵循 Conventional Commits，`type(scope): 中文描述`
  （如 `feat(ssl): 实现基于 SNI 的多证书选择`、`fix(cache): update LastAccess on file cache hit`），
  `git log` 里两种写法都有，但 scope 基本对应 `internal/` 下的包名。
