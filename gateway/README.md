# Portable gateway telemetry extension

This source is based on [HanawaBanana/workbuddy2api](https://github.com/HanawaBanana/workbuddy2api), commit `5755fe3fac45f5fd01c42e583afa0f70d9d6efc1`, matching the revision recorded in the originally bundled `wb2api.exe`. The original MIT license is retained in `LICENSE`.

The portable desktop adds `internal/server/live_usage.go` and its tests, plus small hooks in `handler.go` and `logging.go`. Existing routing and upstream request handling remain the source for ordinary API behavior.

`GET /v1/usage/live` uses the existing API-key authentication and returns only aggregate counters: gateway instance, active requests, exact completed output tokens, estimated active output tokens, and a monotonic observed-output estimate. It does not return or save generated text, prompts, account identifiers, or credentials. The observer reads the same SSE bytes as the existing stream/aggregate paths and does not read ahead or modify responses.

The upstream reports exact token usage at completion. While generating, the HUD estimates one token per four ASCII characters or one non-ASCII character, including reasoning and tool arguments. These are display estimates, not provider billing or tokenizer counts. The HUD marks in-flight totals with `≈`, samples once per second, and replaces estimates with actual completion usage. Waiting, stopped output, idle, disconnected and restarted instances have no stale speed. A startup marker lets the desktop combine earlier log history with the current gateway's exact counters without counting completed requests twice.

Build and verify from this directory with a Go toolchain:

```powershell
go test ./internal/server ./internal/upstream
go build -trimpath -ldflags '-s -w' -o ../wb2api.exe ./cmd/server
```

Then rebuild the Rust desktop so that its embedded gateway matches `../wb2api.exe`. Stop the old gateway before replacing its executable.

## Anonymous free providers

`internal/freegateway` implements the public OpenCode and Kilo contracts
documented by [dsh-our-free-model](https://github.com/Ebony-Vinyl/dsh-our-free-model).
Version 0.1.6 adapts OpenCode compatibility definitions from commit
`8b2d8c298759195aa324e64513fdd19fe25881a7`; attribution and its full MIT notice
are retained in the project-root `NOTICE.md`.
`free_gateways.opencode.enabled` and `free_gateways.kilo.enabled` default to true
in the server configuration. Explicit false disables either source.

Free models use `opencode:` and `kilo:` prefixes and must appear in the current
free catalog before forwarding. Kilo requires `isFree: true`; OpenCode uses its
public free model markers and excludes models that require Messages or Responses.
Requests bypass the WorkBuddy account pool, account session binding, prompt
rewriting and WorkBuddy credentials. The adapter resolves the model prefix and
supports both SSE and non-streaming Chat Completions, with cancellation, bounded
timeouts, isolated catalog caching and per-provider status. The incoming gateway key never leaves
the local server. OpenCode uses the public bearer value; Kilo has no upstream key.

OpenCode requests include the v2 client headers and a stable, OpenCode-formatted
session ID. The compatibility layer supplies the canonical `read` and `shell`
tool declarations required by that upstream. Conflicting client tool names are
temporarily mapped to aliases and restored in SSE and non-streaming responses;
calls to compatibility-only placeholder tools are filtered before reaching the
client. This layer does not execute tools. The supported scope remains Chat
Completions; no Responses or Anthropic protocol conversion is added.
OpenCode always uses upstream streaming; non-streaming callers receive a locally
aggregated JSON completion, while Kilo keeps its original request mode.

`GET /status` distinguishes catalog availability from verified completed inference.
Catalog success does not guarantee inference availability: region, quota,
upstream policy and outages can still produce provider errors, which remain
visible. Kilo free-pool prompts may be logged by the upstream provider. Release
packages contain no user account credentials, local API keys or session data.

The release checks `go test ./cmd/server ./internal/freegateway ./internal/server ./internal/upstream`.
The broader `go test ./...` currently has a pre-existing scheduler test failure:
this portable source archive does not include `scripts/school_open_day_2026.py`.
That legacy activity script is unrelated to the free provider routes.
For explicit live acceptance
of the built binary, from the project root run:

```powershell
python tools/test_free_gateways.py --live
```

The acceptance tool starts an isolated account-free profile, verifies local auth,
free-model routing, unknown-model denial and streaming/non-streaming inference
for the configured free providers, including OpenCode compatibility. It never
reads the user's configuration or accounts. A live result applies to the upstream
state at the time of that run.
