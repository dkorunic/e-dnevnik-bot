# Architecture Overview: e-dnevnik-bot

## 1. System Purpose

**e-dnevnik-bot** is a polling and alerting daemon that monitors the Croatian CARNet [e-Dnevnik](https://ocjene.skole.hr) school portal and delivers grade/exam notifications to parents and students via multiple messaging backends.

**Primary use cases:**

- Notify parents/students immediately when new grades, exams, reading lists, final grades, or national exam results are posted on e-Dnevnik.
- Support multi-user polling (multiple children with separate accounts).
- Deliver alerts across Discord, Telegram, Slack, SMTP mail, WhatsApp, and Google Calendar.

**Target users:** Croatian parents and students; self-hosted via binary or container.

---

## 2. High-Level Architecture

**Style:** Modular monolith with an event-driven internal pipeline. All components run in a single process using goroutines.

**Deployment model:** Single static binary (CGO_ENABLED=0). Runs as a foreground process or systemd-managed daemon. Persistent state in SQLite WAL-mode files.

**External dependencies:**

- CARNet e-Dnevnik portal (SSO/SAML-authenticated, HTML scraped)
- Messaging APIs: Discord, Telegram, Slack, WhatsApp (whatsmeow), Google Calendar (OAuth2), SMTP
- GitHub Releases API (optional version check)

```
┌─────────────────────────────────────────────────────────────────────────┐
│                           e-dnevnik-bot process                         │
│                                                                         │
│  ┌──────────────┐    ┌───────────────────┐    ┌──────────────────────┐ │
│  │  Scraper     │    │   msgDedup        │    │  broadcast.Relay     │ │
│  │  goroutines  │───▶│  (single thread)  │───▶│  (fan-out)           │ │
│  │  (per user)  │    │  SQLite KV check  │    │  ┌────────────────┐  │ │
│  └──────────────┘    └───────────────────┘    │  │ Discord        │  │ │
│                                               │  │ Telegram       │  │ │
│  fetch/ → scrape/ → gradesScraped chan        │  │ Slack          │  │ │
│                        │                     │  │ Mail (SMTP)    │  │ │
│                        ▼                     │  │ Google Cal.    │  │ │
│                    gradesMsg chan             │  │ WhatsApp       │  │ │
│                                               │  └────────────────┘  │ │
│                                               └──────────────────────┘ │
│                                                                         │
│  ┌──────────────┐    ┌───────────────────┐                             │
│  │  SQLite KV   │    │ Persistent queue  │                             │
│  │  (dedup DB)  │    │ (failed msgs)     │                             │
│  └──────────────┘    └───────────────────┘                             │
└─────────────────────────────────────────────────────────────────────────┘
```

---

## 3. Core Components

### `main` (entry point, orchestration)

**Responsibility:** Flag parsing, config loading, polling ticker loop, goroutine lifecycle, systemd integration, profiling, graceful shutdown.
**Key deps:** `peterbourgon/ff/v4`, `iguanesolutions/go-systemd`, `KimMachineGun/automemlimit`
**Patterns:** Signal-context cancellation, atomic error flag (`sync/atomic.Bool`), jitter-based polling ticker, GOMEMLIMIT tuning to 90% of container/host memory.

### `config/`

**Responsibility:** TOML config loading and validation. Each messenger section is independently optional; absence of a section disables that messenger.
**Key deps:** `BurntSushi/toml`, `google/renameio` (atomic file writes)
**Patterns:** Fail-fast validation on startup; all checks in dedicated `checkXConf()` functions; config saved atomically to prevent corruption.

### `fetch/`

**Responsibility:** HTTP client for e-Dnevnik: CSRF token extraction, SAML/SSO auth, class/grades/ICS calendar retrieval.
**Key deps:** `lib4u/fake-useragent` (random Chrome UA per session), standard `net/http` with cookie jar
**Patterns:** Cookie-jar-managed SSO sessions, 120-second timeout (portal is slow), per-session random User-Agent to prevent bot blocking.

### `scrape/`

**Responsibility:** Parses raw HTML from `fetch/` into structured `msgtypes.Message` events.
**Key deps:** `PuerkitoBio/goquery` (jQuery-like HTML parser), `avast/retry-go/v5`
**Patterns:** Retry-wrapped fetch+parse; sends events to `gradesScraped` channel; errors fail the user but don't affect other goroutines.

### `msgtypes/`

**Responsibility:** Shared domain types — `Message` struct and `EventCode` enum (Grade/Exam/Reading/FinalGrade/NationalExam).
**Key deps:** none
**Patterns:** Unified event model across all pipeline stages; `SkipRecipients` field enables partial retry on failure.

### `sqlitedb/`

**Responsibility:** Pure-Go SQLite KV store for deduplication. Keys are SHA-256 hashes of `(username, subject, fields)`. TTL ≈ 1 year.
**Key deps:** `modernc.org/sqlite` (no CGO), `dgraph-io/badger/v4` (migration source only)
**Patterns:** WAL mode, prepared statements, TTL-indexed expiry, automatic BadgerDB migration on first run.

### `queue/`

**Responsibility:** Persistent dead-letter queue for failed message deliveries. Built on top of `sqlitedb`.
**Key deps:** `encdec/` for gob serialization
**Patterns:** Atomic fetch-and-clear semantics; each messenger has its own queue key; resend attempted on next poll cycle.

### `encdec/`

**Responsibility:** `encoding/gob` serialization of `[]msgtypes.Message` for queue persistence.
**Key deps:** standard library only
**Patterns:** Empty input short-circuits without allocating.

### `messenger/`

**Responsibility:** Six independent messenger goroutines, each consuming from a `broadcast.Relay` listener.
**Key deps:** `teivah/broadcast`, `go.uber.org/ratelimit`, per-backend SDK
**Patterns:** Identical lifecycle across all implementations (init → drain queue → process live → store failures). Rate-limited API calls.

| Messenger | Backend Library               | Rate Limit | Format   |
| --------- | ----------------------------- | ---------- | -------- |
| Discord   | `bwmarrin/discordgo`          | 10/min     | Markdown |
| Telegram  | `go-telegram/bot`             | 20/min     | HTML     |
| Slack     | `slack-go/slack`              | 20/min     | Markdown |
| Mail      | `wneessen/go-mail`            | 20/hr      | HTML     |
| Calendar  | `google/google-api-go-client` | 20/min     | Event    |
| WhatsApp  | `go.mau.fi/whatsmeow`         | 10/min     | Plain    |

### `format/`

**Responsibility:** Three message formatters — `plain`, `html`, `markup` (Markdown) — used by different messenger backends.
**Key deps:** none
**Patterns:** Emoji-prefixed headers keyed by `EventCode`; rune-aware string truncation.

### `oauth/`

**Responsibility:** Google Calendar OAuth2 interactive flow: local HTTP server on `:9080`, browser launch, token storage.
**Key deps:** `golang.org/x/oauth2`, `go-chi/chi/v5`, `google/uuid`, `google/renameio`
**Patterns:** UUID state parameter (CSRF protection), atomic token file write (0600), auto-refresh on expiry.

### `logger/`

**Responsibility:** `zerolog` wrapper; global logger with configurable level, JSON or colorized console output.
**Key deps:** `rs/zerolog`, `mattn/go-isatty`

### `version/`

**Responsibility:** Build-time version metadata injected via ldflags.

---

## 4. Data Flow

### Core pipeline

```
[Polling ticker fires]
        │
        ▼
[Per-user goroutine launched]    ← wgScrape (one per user, parallel)
  fetch/ → SAML login
         → get class list
         → get grades HTML + ICS calendar
  scrape/ → parse HTML → Message{...}
         → send to gradesScraped (buffered chan)
        │
        ▼
[wgScrape.Wait() + close(gradesScraped)]
        │
        ▼
[msgDedup goroutine]             ← wgFilter (single goroutine)
  For each Message:
    hash = SHA-256(username + subject + fields)
    if hash exists in SQLite → discard
    if relevance period set and event is stale → discard
    if firstRun → store hash, do NOT forward
    else → store hash, send to gradesMsg (buffered chan)
        │
        ▼
[wgFilter.Wait() + broadcast relay]
        │
        ├──▶ Discord goroutine
        ├──▶ Telegram goroutine
        ├──▶ Slack goroutine
        ├──▶ Mail goroutine
        ├──▶ Calendar goroutine
        └──▶ WhatsApp goroutine
              Each: drain failed queue → send live msgs → queue failures
                    wgMsg.Wait()
```

### Persistence and caching strategy

- **Deduplication DB** (`sqlitedb/`): SHA-256 KV store. All events indexed permanently (~1 year TTL). No read cache; SQLite WAL provides sufficient throughput for single-node polling.
- **Failed message queue** (`queue/`): Per-messenger SQLite keys storing gob-encoded `[]Message`. Cleared atomically on successful re-send.
- **WhatsApp session** (`.e-dnevnik.wa.sqlite`): `whatsmeow`-managed multi-device session database.
- **Calendar OAuth token** (`calendar_token.json`): JSON-encoded `oauth2.Token`, refreshed automatically.
- No in-memory caches (by design — each poll is a fresh HTTP session with cookie jar).

---

## 5. Key Technologies

| Technology              | Reason                                                                          |
| ----------------------- | ------------------------------------------------------------------------------- |
| **Go 1.26+**            | Strong concurrency primitives, static binary, no runtime dependencies           |
| `modernc.org/sqlite`    | CGO-free SQLite — enables fully static binary without C toolchain               |
| `teivah/broadcast`      | Fanout broadcaster: one writer, N concurrent goroutine readers, no shared state |
| `avast/retry-go/v5`     | Declarative retry with context awareness; wraps scraping and messaging          |
| `go.uber.org/ratelimit` | Token-bucket rate limiting per messenger                                        |
| `PuerkitoBio/goquery`   | jQuery-style HTML parsing — e-Dnevnik HTML is complex table-based               |
| `go.mau.fi/whatsmeow`   | Only maintained pure-Go WhatsApp multi-device client                            |
| `peterbourgon/ff/v4`    | Layered flag+env+file configuration with minimal boilerplate                    |
| `rs/zerolog`            | Zero-allocation structured JSON logging                                         |
| `BurntSushi/toml`       | TOML parsing for the config file format                                         |
| PGO (`-pgo=auto`)       | Profile-guided optimization on production builds                                |

---

## 6. Design Patterns & Principles

### Architectural patterns

- **Pipeline with fan-out:** scrape → dedup (single thread for consistency) → broadcast relay → parallel messengers.
- **Dead-letter queue:** failed deliveries persisted to SQLite; retried on next poll without message loss.
- **First-run seeding:** new installations silently seed the dedup DB and suppress alerts — critical UX correctness.

### Concurrency model

- **Per-user goroutines** for scraping (fully independent HTTP sessions).
- **Single-threaded dedup** — intentional; ensures consistent first-run detection and avoids SQLite write contention.
- **Parallel messengers** via broadcast relay — each messenger goroutine gets its own buffered channel listener; a slow API (e.g. WhatsApp) does not block Discord.
- Synchronization via `sync.WaitGroup` and explicit channel close signals (no polling, no sleeps).

### Error handling

- **Fail-fast on startup:** config validation, DB init, and messenger credential checks are all fatal.
- **Isolated runtime failures:** a scraping failure for one user sets an atomic error flag but does not cancel other users.
- **Graceful degradation:** messenger failures store messages to queue; partial delivery tracked via `SkipRecipients`.
- **Error flag propagation:** `atomic.Bool exitWithError` — goroutines write it, main reads it at shutdown to set exit code.

### Domain modeling

- `msgtypes.Message` is the canonical domain event. It is created in `scrape/`, filtered in `routines.go`, formatted by `format/`, and serialized by `encdec/`. No package crosses this boundary except through the `Message` type.

---

## 7. Scaling & Performance Considerations

**Single-node only.** There is no horizontal scaling model — the design assumes one process per household/installation.

**Vertical considerations:**

- Memory bounded via `automemlimit` (GOMEMLIMIT = 90% of available/cgroup memory). Safe for container deployment.
- CPU bounded by number of users × scraping latency. With retry defaults, a 10-user config could take several minutes per cycle, which is fine for 1-hour polling intervals.

**Bottlenecks:**

- e-Dnevnik portal latency (120-second HTTP timeout reflects real-world slowness).
- WhatsApp link reliability — `whatsmeow` reconnects aggressively but the protocol is unofficial.
- SQLite write throughput is not a concern at this polling scale.

**Async processing:**

- Version check runs in a separate `wgVersion` goroutine — never delays the scrape cycle.
- ±10% jitter on polling interval prevents two instances started simultaneously from hammering the portal together.

**PGO:** Production builds use `-pgo=auto` for CPU-profile-guided optimization. The profile is expected to live in the repo root (Go toolchain auto-discovery).

---

## 8. Security Considerations

### Authentication & authorization

- SAML/SSO auth to e-Dnevnik uses a fresh cookie jar per session; no tokens persisted.
- WhatsApp session key material stored in `.e-dnevnik.wa.sqlite` (local file, user permissions).
- Google Calendar OAuth2 token stored in `calendar_token.json` (written 0600 via `renameio`).
- OAuth2 state parameter is a UUID — prevents CSRF on the local callback server.

### Credential storage

- TOML config file contains plain-text passwords. The application does not enforce file permissions; **operators must set 0600 manually** (assumed convention, not enforced).
- Bot tokens (Discord, Telegram, Slack) stored as plain strings in the TOML config.

### Input validation

- Config validated at startup; invalid tokens/IDs cause fatal exit.
- HTML parsed via `goquery` (no `innerHTML`/eval risk in a Go context).
- SQL queries use prepared statements — no string interpolation.

### Secrets management

- No secrets management system (Vault, etc.) — this is a personal tool, not enterprise software.
- Google Calendar credentials are embedded at build time via the `oauth/` package assets.

### Network

- All external calls use HTTPS (OAuth2 endpoints, messaging APIs).
- 120-second timeout on HTTP client limits exposure to slowloris-style hangs.
- Random User-Agent per session reduces portal fingerprinting risk.

---

## 9. Observability

### Logging

- `rs/zerolog` structured JSON by default; colorized console via `-l` flag.
- Log level: configurable (`-v` for debug; `LOG_LEVEL` env var).
- Caller info included in all log entries.
- WhatsApp and Calendar initialization events logged in detail.

### Systemd integration

- `sd_notify` ready/stopping signals.
- Watchdog heartbeat sent each poll cycle — allows systemd to restart a hung process.
- Status string updated with human-readable poll interval.

### Profiling (opt-in)

- CPU profile: `-c <file>` flag → pprof format.
- Heap profile: `-m <file>` flag → written on process exit after forced GC.

### Metrics

- **None.** No Prometheus/StatsD/OTEL instrumentation. _(Personal tool, not cloud-monitored.)_

### Distributed tracing

- **None.** Single-process, no distributed tracing needed.

### Version checking

- Polls GitHub Releases API on each cycle (parallel, non-blocking).
- Skips check if binary is a dev/dirty build.
- Logs a warning if a newer semver tag exists.

---

## 10. Risks & Trade-offs

| Area                                      | Risk / Trade-off                                                                                                                                |
| ----------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------- |
| **Plain-text credentials in TOML**        | Bot tokens and passwords readable by any process running as the same user. Acceptable for personal use; problematic in shared environments.     |
| **Unofficial WhatsApp protocol**          | `whatsmeow` can break on WhatsApp protocol updates, is not officially supported, and temporary bans are a real risk for high-frequency senders. |
| **No horizontal scaling**                 | Two instances polling the same account simultaneously will cause duplicate alerts (no distributed lock).                                        |
| **No metrics/alerting on the bot itself** | Failures are logged but not surfaced to an external dashboard. A silently hung bot won't be noticed until grades are missed.                    |
| **SQLite as shared state**                | WAL mode is robust, but all scrapers must access the same file. Docker volume mounts on NFS/FUSE can cause corruption.                          |
| **Embedded Google credentials**           | OAuth2 client ID/secret compiled into binary. Binary should be treated as a secret if Google credentials are sensitive.                         |
| **First-run behavior**                    | If the DB is deleted accidentally, the next run silently re-seeds without sending alerts. Users may think the bot missed events.                |
| **Retry queue is best-effort**            | If the process crashes mid-delivery, messages in-flight (not yet queued) are lost. Queue is only populated after a confirmed API error.         |
| **BadgerDB migration is destructive**     | Old BadgerDB directory deleted after import. No rollback if import fails partway.                                                               |
| **Go version pinned to 1.26+**            | Cutting-edge; some CI environments may not have 1.26 toolchain.                                                                                 |
