# e-dnevnik-bot OpenCode Guidance

## Prerequisites

Load-bearing context lives in **[CLAUDE.md](CLAUDE.md)** (invariants, constraints) and **[ARCHITECTURE.md](ARCHITECTURE.md)** (full design). This file is the compact quick reference.

## Toolchain

- **Go 1.27+** — mandatory; set by the `go.mod` pin (the source itself needs 1.26: `errors.AsType`, `new(expr)`)
- Static binary via `CGO_ENABLED=0` (enforced in `Taskfile.yml` env); do not override
- `modernc.org/sqlite` at runtime — no C toolchain needed

## Commands

```bash
task update        # go get -u + realign modernc.org/libc + go mod tidy (currently breaks the build, see below)
task update-major  # list available major-version upgrades
task update-tools  # install gci, gofumpt, betteralign
task tools         # install whichever of those is missing
task fmt           # go mod tidy → gci → gofumpt → go fix → betteralign -apply
task modernize     # gopls/modernize fixes across the tree
task build         # fmt → static build with ldflags
task build-debug   # fmt → race build (CGO on). Use for races only.
task lint           # fmt → golangci-lint (5m timeout)
task lint-nil       # fmt → nilaway. Separate pass, NOT part of `task lint`.
task test           # go test ./...
```

Pre-commit: `task fmt` must pass. The three external formatters (gci, gofumpt, betteralign) must be installed; `go fix` ships with Go and may rewrite code you did not edit.

Single test: `go test -run TestName ./path/to/package/`

After `task update`, run `go get github.com/quic-go/quic-go@v0.64.1`: v0.65.0 removed an API that `enetx/http3` v1.0.9 still calls.

Docs: a change to behaviour, flags, config keys, packages or tasks updates `README.md`, `CLAUDE.md`, `AGENTS.md`, `ARCHITECTURE.md` and `.e-dnevnik.toml.example` in the same commit.

## Pre-commit gotchas

- `betteralign -apply` reorders struct fields to minimise padding — do not revert
- `task build`, `task build-debug`, `task lint` and `task lint-nil` all run `task fmt` first

## Binary flags

| Flag | Meaning |
|--------|---------|
| `-t` / `--test` | Synthetic message through full pipeline (verify messenger creds without scraping) |
| `-0` / `--fulldebug` | Log every scraped event before dedup; implies `-v` |
| `-d` / `--daemon` | Continuous polling mode |
| `-j` / `--jitter` | ±10% continuous jitter on poll interval |
| `-c <file>` / `-m <file>` | CPU / heap pprof profiles |
| `-i <duration>` | Poll interval (minimum 1h) |
| `-p <duration>` | Relevance period for grades and exams (default `720h`, `0` = unlimited) |
| `-q <HH:MM-HH:MM>` / `--quiethours` | Daily local-time window with no polls, daemon mode only (default `23:00-06:00`, `off` disables) |
| `--readinglist` | Process reading list events |
| `--exams` / `--nationalexams` / `--digest` | Default true; `=false` suppresses those alerts (suppressed events are still dedup-flagged) |
| `--driftcheck` | Cross-check scrapes against the portal's own averages; off by default |
| `-r <uint>` | Retry attempts on error (default: 3) |

## Load-bearing invariants

- **Shutdown-tolerant queue writes** (`queue/queue.go:StoreFailedMsgs`): the store detaches a cancelled ctx itself; messengers call it via `storeQueued` with the raw `ctx`. Do not move the detach back to call sites
- **Two-level WaitGroup** (`routines.go:msgSend`): close every messenger channel *then* `wgInner.Wait()` — reversed order deadlocks. Fan-out is hand-rolled non-blocking (`dispatch()`: `select`/`default` → `storeOverflow` spill-to-queue), not `teivah/broadcast`
- **Messenger panic guard** (`messenger/*.go`): entry points are `(err error)` with `defer recoverMessenger(..., inflight)`; set `inflight = &g` before processing a live msg, nil after / on the resend path, so a panic requeues it. Never hold a mutex across panic-able code under the guard (leaked lock hangs next cycle — see `ensureCalendarInit`)
- **Poison-drop permanent errors** (`messenger/*.go`): `markNamePermanent` wraps `retry.Unrecoverable(permanentError{err})` — the inner sentinel survives `retry.Do` stripping so `isPermanentSendErr` detects it post-loop; permanent per-recipient failures are logged + skipped, not requeued
- **Config errors are queued, never dropped** (`markConfigErr`): a bad token or login stops the cycle and keeps the message. Never classify an auth failure as permanent. Webhook endpoints are independent, so nothing there stops the cycle
- **Outbox** (`routines.go`, `queue/queue.go`): `CheckAndFlagOutbox` writes the flag and outbox row in one transaction, so a hard kill cannot lose a flagged event; `enabledQueues` must stay in step with `msgSend`
- **Dedup identity** (`scrape/helpers.go:cellValues`): `Message.Fields` is hashed, so changing which cells it emits re-alerts every stored event once; a changed class count does not (`classAliases`, `routines.go`)
- **Single-threaded dedup** (`routines.go:msgDedup`): do not parallelize
- **First-run seeding is silent, per user** (`seed.go` + `msgDedup`): do not suppress
- **Digest and drift alerts bypass dedup** (`digest.go`, `drift.go`): a failed scrape must not claim a digest week or count as a quiet one
- **TTL-based dedup re-fires** after ~1 year: `sqlitedb/db.go:CheckAndFlagTTL`
- **Continuous jitter** `[0.9, 1.1)`: `main.go:durationRandJitter` — do not discretize
- **Bounded version check**: `versionCheckTimeout = 30s` in `routines.go:versionCheck`

## Runtime constraints

- **WhatsApp first run**: interactive QR or phone PIN pairing (`.e-dnevnik.wa.sqlite`). Cannot be automated.
- **Google Calendar first run**: local HTTP on `:9080` or a free port, browser OAuth2 consent with PKCE (`calendar_token.json`). OAuth clients are embedded.
- **Poll interval floor**: clamped to 1h in `flags.go`.
- **Scrape budget**: at least 20 minutes per user, per cycle.
- **Credentials**: read once at startup; rotating one needs a restart.
- **GOMEMLIMIT**: auto-tuned to 90% of cgroup/system memory.
- **File permissions**: `LoadConfig` best-effort tightens the config to 0600 on every load (warn-only on failure); `SaveConfig` writes 0600. Both SQLite stores are prepared the same way (`sqlitedb.PrepareFile`).
- **Config rewrites**: WhatsApp JID resolution and Telegram chat-ID remap rewrite the config file in place via `LoadConfigRaw` (non-validating decode — a broken mid-run edit must not `logger.Fatal` a messenger goroutine). Comments and key ordering are not preserved.

## CARNet portal constraints

- Blocks non-Croatian IPs — bot must run inside Croatia.
- 120-second HTTP timeout per request.
- Impersonates Chrome 152 through one `enetx/surf` profile: never hand-set browser-identifying headers. `SecureTLS()` is mandatory (`fetch/chrome.go`).
- SAML/SSO via cookie jar, no token persistence.
