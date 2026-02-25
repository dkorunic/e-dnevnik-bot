# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build & Development Commands

This project uses [Task](https://taskfile.dev/) (Taskfile.yml) as the build system.

```bash
task build          # fmt + static build with PGO and ldflags (CGO_ENABLED=0)
task build-debug    # fmt + race detector build (CGO_ENABLED=1)
task test           # go test ./...
task lint           # fmt + golangci-lint --timeout 5m
task fmt            # go mod tidy + gci write + gofumpt + betteralign -apply
task update         # go get -u + go mod tidy
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
- **Subsequent runs**: only new (unseen) events pass through the dedup filter.
- Failed message deliveries are stored in a persistent queue (via `queue/` + `sqlitedb/`) and retried.

### Package Layout

| Package | Role |
|---|---|
| `main` | Entry point, flag parsing (`peterbourgon/ff`), main ticker loop, goroutine orchestration, systemd sd_notify/watchdog |
| `config/` | TOML config loading (`BurntSushi/toml`), type definitions, token/ID validators |
| `fetch/` | HTTP client for e-Dnevnik: CSRF token extraction, SSO/SAML auth, grades/exams/courses fetching. Uses random per-session User-Agent. |
| `scrape/` | Parses HTML responses from `fetch/` into `msgtypes.Message` events (grades, exams, reading lists, final grades, national exams) |
| `msgtypes/` | Shared `Message` struct with `EventCode` enum (Grade/Exam/Reading/FinalGrade/NationalExam) |
| `messenger/` | Implementations for Discord, Telegram, Slack, Mail (SMTP), Google Calendar, WhatsApp. Each reads from a `broadcast.Relay` listener. |
| `sqlitedb/` | Pure-Go SQLite KV store (`modernc.org/sqlite`) in WAL mode. Keys are SHA-256 hashes of (username, subject, fields). TTL ≈ 1 year. Handles migration from legacy BadgerDB. |
| `queue/` | Persistent failed-message queue built on top of `sqlitedb.FetchAndStore` |
| `format/` | Message formatters: `plain`, `html`, `markup` (Markdown) used by various messengers |
| `encdec/` | Protobuf encoding/decoding of `[]msgtypes.Message` for queue persistence |
| `oauth/` | Google Calendar OAuth2 flow (browser-based, stores token to JSON file) |
| `logger/` | Zerolog wrapper supporting JSON (default) or colorized console output |
| `version/` | Build-time version info |

### Key Design Details

- **CGO_ENABLED=0**: fully static binary; SQLite via pure-Go `modernc.org/sqlite`.
- **PGO**: production build uses `-pgo=auto` for profile-guided optimization.
- **Concurrency**: all per-user scrapers run in parallel (`wgScrape.Go`); all messengers run in parallel via `teivah/broadcast` relay.
- **Jitter**: daemon mode applies ±10% random jitter to poll interval to avoid thundering herd.
- **Retry**: `avast/retry-go` wraps scraping and messaging operations; default 3 attempts.
- **Rate limiting**: `go.uber.org/ratelimit` used in messenger implementations.
- **WhatsApp**: uses `go.mau.fi/whatsmeow` (multi-device), requires interactive pairing on first run (QR code or phone number PIN). Stores session in `.e-dnevnik-whatsapp.db.sqlite`.
- **Google Calendar**: OAuth2 flow requires interactive browser on first run; token persisted to `calendar_token.json`.
- **Database migration**: automatically imports legacy BadgerDB data into SQLite on first run after upgrade.

### Configuration

Config file is TOML (`.e-dnevnik.toml`). Multiple `[[user]]` blocks are supported. Each messaging backend has its own `[section]` and is enabled only when that section is present and valid (validated in `config/config.go`).

### Linting

`golangci-lint` runs nearly all linters (see `.golangci.yml`). Notable disabled linters: `cyclop`, `funlen`, `mnd`, `varnamelen`, `wrapcheck`. Test files are excluded from most checks.
