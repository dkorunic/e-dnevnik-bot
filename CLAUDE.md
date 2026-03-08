# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build & Development Commands

This project uses [Task](https://taskfile.dev/) (Taskfile.yml) as the build system.

```bash
task build          # fmt + static build with PGO and ldflags (CGO_ENABLED=0)
task build-debug    # fmt + race detector build (CGO_ENABLED=1)
task test           # go test ./...
task lint           # fmt + golangci-lint --timeout 5m
task lint-nil       # fmt + nilaway (nil-safety analysis)
task fmt            # go mod tidy + gci write + gofumpt + betteralign -apply
task modernize      # apply gopls modernize fixes across the codebase
task update         # go get -u + go mod tidy
task update-major   # list available major-version upgrades (gomajor)
```

To run a single test:
```bash
go test -run TestName ./path/to/package/
```

Three formatting tools are required and must all pass before committing:
- `gci` — import ordering
- `gofumpt` — stricter gofmt
- `betteralign` — struct field alignment optimization

Install them with: `task update-tools`

The build injects version variables via ldflags: `GitTag`, `GitCommit`, `GitDirty`, `BuildTime`.

Opt-in profiling flags: `-c <file>` writes a CPU pprof profile; `-m <file>` writes a heap profile on exit (after forced GC).

## Architecture Overview

e-dnevnik-bot is a polling/alerting daemon that scrapes the Croatian CARNet e-Dnevnik school portal and delivers notifications via multiple messaging backends.

### Data Pipeline

```
scrapers (per-user goroutines)
    → gradesScraped channel
        → msgDedup (SQLite dedup check, first-run seeding)
            → gradesMsg channel
                → msgSend broadcast relay
                    → Discord / Telegram / Slack / Mail / Google Calendar / WhatsApp goroutines
```

- **First run** (no existing DB): seeds the database with all seen events but sends **no alerts**, preventing flooding.
- **Subsequent runs**: only new (unseen) events pass through the dedup filter. An optional relevance period can also discard stale events.
- Failed message deliveries are stored in a persistent queue (via `queue/` + `sqlitedb/`) and retried at the start of the next poll cycle.

### Package Layout

| Package | Role |
|---|---|
| `main` | Entry point, flag parsing (`peterbourgon/ff`), main ticker loop, goroutine orchestration, systemd sd_notify/watchdog |
| `config/` | TOML config loading (`BurntSushi/toml`), type definitions, token/ID validators. Absence of a messenger section disables that messenger. |
| `fetch/` | HTTP client for e-Dnevnik: CSRF token extraction, SSO/SAML auth, grades/exams/courses fetching. Fresh cookie jar + random User-Agent per session. 120 s timeout. |
| `scrape/` | Parses HTML responses from `fetch/` (via `goquery`) into `msgtypes.Message` events; retry-wrapped. |
| `msgtypes/` | Shared `Message` struct with `EventCode` enum (Grade/Exam/Reading/FinalGrade/NationalExam). `SkipRecipients` enables partial retry. |
| `messenger/` | Six messenger goroutines (Discord, Telegram, Slack, Mail, Google Calendar, WhatsApp). Identical lifecycle: init → drain queue → process live → store failures. |
| `sqlitedb/` | Pure-Go SQLite KV store (`modernc.org/sqlite`) in WAL mode. Keys are SHA-256 hashes of (username, subject, fields). TTL ≈ 1 year. Handles migration from legacy BadgerDB. |
| `queue/` | Persistent dead-letter queue for failed deliveries, built on `sqlitedb`. Per-messenger keys; cleared atomically on successful re-send. |
| `format/` | Message formatters: `plain`, `html`, `markup` (Markdown). Emoji-prefixed headers keyed by `EventCode`; rune-aware truncation. |
| `encdec/` | `encoding/gob` serialization of `[]msgtypes.Message` for queue persistence. |
| `oauth/` | Google Calendar OAuth2 flow: local HTTP server on `:9080`, UUID CSRF state, atomic 0600 token file write. |
| `logger/` | Zerolog wrapper (JSON default; colorized console via `-l`). |
| `version/` | Build-time version info injected via ldflags. |

### Key Design Details

- **CGO_ENABLED=0**: fully static binary; SQLite via pure-Go `modernc.org/sqlite`.
- **PGO**: production build uses `-pgo=auto`; profile expected in repo root (auto-discovered by toolchain).
- **Concurrency**: per-user scrapers run in parallel (`wgScrape.Go`); dedup runs single-threaded (intentional — ensures consistent first-run detection and avoids SQLite write contention); messengers run in parallel via `teivah/broadcast` relay (slow messenger does not block others).
- **Error propagation**: `atomic.Bool exitWithError` — scraper goroutines set it on failure; main reads it at shutdown to set exit code. Failures are isolated per user.
- **Jitter**: daemon mode applies ±10% random jitter to poll interval to avoid thundering herd.
- **Retry**: `avast/retry-go` wraps scraping and messaging operations; default 3 attempts.
- **Rate limiting**: `go.uber.org/ratelimit` in each messenger. Limits: Discord 10/min, Telegram 20/min, Slack 20/min, Mail 20/hr, Google Calendar 20/min, WhatsApp 10/min.
- **GOMEMLIMIT**: `automemlimit` sets Go memory limit to 90% of available/cgroup memory at startup.
- **WhatsApp**: uses `go.mau.fi/whatsmeow` (multi-device), requires interactive pairing on first run (QR code or phone number PIN). Session stored in `.e-dnevnik.wa.sqlite`.
- **Google Calendar**: OAuth2 flow requires interactive browser on first run; token persisted to `calendar_token.json`.
- **Version check**: GitHub Releases API polled each cycle in a parallel `wgVersion` goroutine; skipped for dev/dirty builds.
- **Database migration**: automatically imports legacy BadgerDB data into SQLite on first run after upgrade. BadgerDB directory deleted after import — no rollback.

### Configuration

Config file is TOML (`.e-dnevnik.toml`). Multiple `[[user]]` blocks are supported. Each messaging backend has its own `[section]` and is enabled only when that section is present and valid (validated in `config/config.go`). Config file should be set to 0600 — the application does not enforce this.

### Linting

`golangci-lint` runs nearly all linters (see `.golangci.yml`). Notable disabled linters: `cyclop`, `funlen`, `mnd`, `varnamelen`, `wrapcheck`. Test files are excluded from most linter checks.
