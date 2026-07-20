# e-dnevnik-bot OpenCode Guidance

## Prerequisites

Load-bearing context lives in **[CLAUDE.md](CLAUDE.md)** (invariants, constraints) and **[ARCHITECTURE.md](ARCHITECTURE.md)** (full design). This file is the compact quick reference.

## Toolchain

- **Go 1.26+** — mandatory; code uses `sync.WaitGroup.Go` (Go 1.25) and `context.WithoutCancel` (Go 1.21)
- Static binary via `CGO_ENABLED=0` (enforced in `Taskfile.yml` env); do not override
- `modernc.org/sqlite` at runtime — no C toolchain needed

## Commands

```bash
task update        # go get -u + go mod tidy
task update-tools  # install gci, gofumpt, betteralign
task fmt           # go mod tidy → gci → gofumpt → betteralign -apply
task modernize     # gopls/modernize fixes across the tree
task build         # fmt → static PGO build with ldflags
task build-debug   # fmt → race build (CGO on). Use for races only.
task lint           # fmt → golangci-lint (5m timeout)
task lint-nil       # fmt → nilaway. Separate pass, NOT part of `task lint`.
task test           # go test ./...
```

Pre-commit: `task fmt` must pass. All three formatters (gci, gofumpt, betteralign) must be installed.

Single test: `go test -run TestName ./path/to/package/`

## Pre-commit gotchas

- `betteralign -apply` reorders struct fields to minimise padding — do not revert
- `task build` runs `task fmt` implicitly; `task build-debug` runs `task update` then `task fmt`

## Binary flags

| Flag | Meaning |
|--------|---------|
| `-t` / `--test` | Synthetic message through full pipeline (verify messenger creds without scraping) |
| `-0` / `--fulldebug` | Log every scraped event before dedup; implies `-v` |
| `-d` / `--daemon` | Continuous polling mode |
| `-j` / `--jitter` | ±10% continuous jitter on poll interval |
| `-c <file>` / `-m <file>` | CPU / heap pprof profiles |
| `-i <duration>` | Poll interval (minimum 1h) |
| `--readinglist` | Process reading list events |
| `-r <uint>` | Retry attempts on error (default: 3) |

## Load-bearing invariants

- **Shutdown-tolerant queue writes** (`messenger/common.go`): use `queueStoreCtx`, not raw `ctx`
- **Two-level WaitGroup** (`routines.go:msgSend`): close every messenger channel *then* `wgInner.Wait()` — reversed order deadlocks. Fan-out is hand-rolled non-blocking (`select`/`default` → `storeOverflow` spill-to-queue), not `teivah/broadcast`
- **Single-threaded dedup** (`routines.go:msgDedup`): do not parallelize
- **First-run seeding is silent** (`sqlitedb/db.go` + `msgDedup`): do not suppress
- **TTL-based dedup re-fires** after ~1 year: `sqlitedb/db.go:CheckAndFlagTTL`
- **Continuous jitter** `[0.9, 1.1)`: `main.go:durationRandJitter` — do not discretize
- **Bounded version check**: `versionCheckTimeout = 30s` in `routines.go:versionCheck`

## Runtime constraints

- **WhatsApp first run**: interactive QR or phone PIN pairing (`.e-dnevnik.wa.sqlite`). Cannot be automated.
- **Google Calendar first run**: local HTTP on `:9080`, browser OAuth2 consent (`calendar_token.json`).
- **Poll interval floor**: clamped to 1h in `flags.go`.
- **GOMEMLIMIT**: auto-tuned to 90% of cgroup/system memory.
- **Config file permissions**: the bot does **not** enforce 0600 — operator responsibility.
- **Config rewrites**: WhatsApp JID resolution can rewrite the config file in place. Comments and key ordering are not preserved.

## CARNet portal constraints

- Blocks non-Croatian IPs — bot must run inside Croatia.
- 120-second HTTP timeout per session.
- Random User-Agent per session.
- SAML/SSO via cookie jar, no token persistence.
