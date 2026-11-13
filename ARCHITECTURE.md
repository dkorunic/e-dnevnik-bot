# Architecture: e-dnevnik-bot

A single-process, self-hosted daemon that scrapes the CARNet [e-Dnevnik](https://ocjene.skole.hr) portal for one or more users and fans new events out to messaging and calendar backends. It ships as a static binary (`CGO_ENABLED=0`) or container. State lives in local SQLite (WAL) files.

This file describes the design. Rules that must not be broken are in [CLAUDE.md](CLAUDE.md); user-facing behaviour is in [README.md](README.md).

## 1. Overview

```
                        one poll cycle (runPollCycle), on a fresh DB handle
 ┌──────────────┐ gradesScraped ┌────────────────┐ gradesMsg ┌────────────────────┐
 │ scrapers     │──────────────▶│ msgDedup       │──────────▶│ msgSend fan-out    │
 │ 1 per user   │  buffered 500 │ 1 goroutine    │ buffered  │ non-blocking       │
 │ fetch+scrape │               │ flag + outbox  │ 500       │ 100-slot per sink  │
 └──────┬───────┘               └───────┬────────┘           └─────────┬──────────┘
        │                               │                              ▼
   e-Dnevnik portal                     ▼                  Discord  Telegram  Slack
   (SAML/SSO, HTML, ICS)        SQLite KV store            Mail  WhatsApp  Webhook
                                dedup, outbox, queues      Google Calendar  CalDAV
                                seed/digest/drift state    (calendars: exams only)
```

External systems: the portal, the eight backends, and the GitHub Releases API (version check).

## 2. Components

Root package `main`:

| File | Role |
|---|---|
| `main.go` | Startup, ticker loop, `runPollCycle`, `-t` emulation, systemd, profiling, shutdown |
| `flags.go` | CLI flags (`peterbourgon/ff/v4`, flags only) as package-level pointers, and their clamps |
| `routines.go` | `scrapers`, `msgDedup`, `msgSend`, `recoverOutbox`, `versionCheck` |
| `seed.go` | Per-user first-run markers |
| `digest.go` | Weekly exam digest; `userSet` of failed and drifted users per cycle |
| `drift.go` | Two-scrape confirmation and weekly alert for portal drift |
| `quiet.go` | `-q` window, `nextPollDelay`, `pollRuns` |
| `init.go` | Interactive WhatsApp pairing and Calendar OAuth setup |
| `db.go`, `log.go` | Database open/close; log setup |

`internal/`:

| Package | Role | Main dependencies |
|---|---|---|
| `msgtypes` | `Message` and `EventCode` (Grade, Exam, Reading, FinalGrade, NationalExam, ExamDigest, PortalDrift) | none |
| `fetch` | Portal HTTP client: SAML/SSO login, class list, grades, ICS. 120 s per request. Impersonates Chrome 158 | `enetx/surf` |
| `scrape` | HTML to `Message`; retry; per-user budget of at least 20 min; drift checks (`drift.go`) | `goquery`, `retry-go/v5` |
| `sqlitedb` | KV store on one `kv(key, value, expires_at)` table; 4 connections; expired-row sweep on open | `modernc.org/sqlite` |
| `codec` | CBOR encoding of queued messages | `fxamacker/cbor/v2` |
| `queue` | Per-messenger failed-message queues and the outbox | `sqlitedb`, `codec` |
| `messenger` | The eight backends and their shared send logic (`common.go`) | one SDK per backend, `go.uber.org/ratelimit` |
| `format` | One renderer, three escapers: plain, Telegram HTML, Slack mrkdwn | none |
| `oauth` | Google OAuth2 with PKCE: local callback on `:9080` or a free port, token storage | `x/oauth2`, `go-chi/chi/v5`, `google/uuid`, `renameio` |
| `config` | TOML load, fail-fast validation (`check*Conf`), atomic `0600` writes | `BurntSushi/toml`, `renameio` |
| `logger` | Global logger | `rs/zerolog` |
| `version` | Dependency versions from the binary's build info | none |

## 3. Poll cycle

```
ticker fires (re-armed past quiet hours by nextPollDelay; first poll always runs, held inside the window)
  versionCheck                 background, 30 s limit; skipped for local/dirty builds
  openDB                       on failure: skip the cycle, latch exitWithError
  recoverOutbox                a killed or held cycle's rows move to every enabled queue
  scrapers                     one goroutine per user; failures recorded in userSet
  msgDedup                     single goroutine, for each event:
    collect exams for the digest (before dedup)
    key = SHA-256(username, subject, Fields)     Average is appended after this
    CheckAndFlagOutbox(key)                      one transaction:
      forward = not first run for this user
            and code not suppressed (--readinglist, --exams, --nationalexams)
            and not older than -p                (undated or unparsable: passes)
      flag the key with TTL; if forward, also write the outbox row
    forward only if the key was not already live
  after scrapers close:
    markSeeded                 users whose scrape succeeded
    sendDigests                at most one per user per week; bypasses dedup
    sendDriftAlerts            with --driftcheck; bypasses dedup
  msgSend                      dispatch() to each sink; a full buffer spills to its queue
                               held cycle: holdAlerts only drains, digest not claimed
    each messenger: resend queue, drop expired, send live, requeue failures
  wait for all stages
  ClearOutbox                  unless a queue write failed this cycle or it was held
```

## 4. Persistence

`.e-dnevnik.db.sqlite` holds everything in the `kv` table:

| Key | Value | Purpose |
|---|---|---|
| SHA-256 of `(username, subject, fields)` | marker | Dedup, expires after `DefaultEntryTTL` (9000 h); an older separator-less hash is still honoured |
| `<name>-queue\x00<time><seq>` | CBOR message | Per-messenger failed-message queue, one row per message, 30-day `MaxQueueAge` |
| `outbox\x00<time><seq>` | CBOR message | Events flagged but not yet delivered or queued, or held by a first poll in quiet hours |
| `seeded\x00<user>`, `seeded-migrated` | marker | Per-user first-run state; one-time migration of older databases |
| `digest-week\x00<user>` | Monday | Digest already claimed for that week |
| `portal-drift\x00<user>` | state | Drift confirmation and weekly alert limit |

- Queue rows are read without being removed and deleted only after their outcome is durable, so delivery is at-least-once. Legacy aggregate rows are migrated and multi-message rows split on fetch.
- `.e-dnevnik.wa.sqlite` is the `whatsmeow` session. `calendar_token.json` is the OAuth2 token, with the issuing `client_id`.
- There is no in-memory cache between cycles.

## 5. Messengers

Each backend is one goroutine draining its own channel: resend the queue, send live messages, requeue failures. The contract is in CLAUDE.md.

| Backend | Library | Limit | Format | Size cap |
|---|---|---|---|---|
| Discord | `bwmarrin/discordgo` | 10/min | Embed | 6000 chars |
| Telegram | `go-telegram/bot` | 20/min | HTML | 4096 chars |
| Slack | `slack-go/slack` | 20/min | mrkdwn | 3000 chars |
| Mail | `wneessen/go-mail` | 20/h, 15 per cycle | HTML + plain | 256-char subject |
| WhatsApp | `go.mau.fi/whatsmeow` | 10/min | Plain | 4096 chars |
| Webhook | `net/http` | 12/min | JSON or plain | none |
| Google Calendar | `google.golang.org/api` | 20/min | All-day event | n/a |
| CalDAV | `net/http` + `ical.go` | 30/min | All-day event | n/a |

Send failures fall into these classes:

| Class | Example | Handling |
|---|---|---|
| Transient | timeout, 429, 5xx | Retried, then queued |
| Permanent | blocked bot, deleted chat | Dropped for that recipient |
| Config | bad token or login | Retries stop, queued, rest of the cycle queued unsent |
| Unreachable | connection refused | Retries spent once, then rest of the cycle queued unsent |
| Unknown reach | unlisted Slack code, ambiguous mail 5xx | That recipient queued, loop continues |

Webhook endpoints are independent, so none of its failures stop the cycle.

- `SkipRecipients` records who has been handled, so a requeued message does not repeat for them.
- Calendar events have deterministic IDs and create-only writes: a Google 409 or CalDAV 412 counts as success.
- `CalendarDeferred` is a queue-only stub used when Calendar has no token yet.
- A send-path panic is contained by `recoverMessenger`, which requeues the in-flight message and drains the channel.

## 6. Concurrency and shutdown

- Dedup is deliberately a single goroutine. Fan-out never blocks: a slow messenger spills to its queue.
- `msgSend` closes every sink channel, then waits on `wgInner`.
- A cycle runs to completion, bounded by the scrape budget and by `stopsCycle` and `MailCycleLimit` for sends.
- On SIGTERM the context is cancelled. Queue writes detach from it (`context.WithoutCancel`, 5 s). Unflagged events are simply scraped again.
- The systemd watchdog runs in `bgWG`, awaited for at most `exitDelay` (10 s).
- `exitWithError` latches for the process lifetime and sets the exit status.
- Poll jitter is continuous over `[0.9, 1.1)`. The poll interval has a 1 h floor.

## 7. Security

- **Portal:** TLS verification is forced with `SecureTLS()`, since surf skips it by default. A cookie jar holds the session; nothing is persisted.
- **Credentials at rest:** plaintext in the TOML file, tightened to `0600` on load. Both SQLite stores and the OAuth token are `0600`.
- **Google OAuth:** client credentials are embedded from `internal/messenger/assets/`. New setups use a desktop client with PKCE and a UUID state; older installs stay on the legacy web client, selected by the `client_id` in the token file.
- **CalDAV:** Basic Auth, `https` except to loopback, redirects refused so credentials cannot be replayed.
- **Webhook:** `http` allowed with a warning for non-loopback hosts; redirects refused; only scheme and host are logged, because the path often holds the secret.
- **Data handling:** events are named by user, subject and code at Info and above. Grade content is logged only with `-0`.
- **SQL** uses bound parameters. HTML is only parsed with `goquery`, never executed.

## 8. Observability

- `zerolog` JSON, or console with `-l`. Level from `-v` or `LOG_LEVEL`.
- systemd: `sd_notify` ready, stopping and status, plus a watchdog heartbeat from an independent ticker. The heartbeat shows the process is alive, not that a poll cycle is progressing.
- `-c` and `-m` write pprof profiles. A cycle uses about 10 ms of CPU, so PGO is not used.
- A newer release is reported at Info level. There are no metrics or tracing.

## 9. Trade-offs

| Area | Consequence |
|---|---|
| Single node | Two instances on one account duplicate alerts; there is no lock. |
| Portal scraping | Layout changes can silently break parsing; `--driftcheck` is the opt-in guard. Portal latency dominates cycle time. |
| Hard kill (SIGKILL, OOM, power) | The outbox prevents loss, but delivered rows may repeat, since nothing records which messengers finished. The digest row is not in its claim's transaction. |
| Deleted database | The next run silently re-seeds with no alerts. |
| Dedup TTL | After about a year, an event still on the portal alerts again. |
| Identity | `Message.Fields` and the subject are the identity. Changing what is scraped re-alerts once; a student's class count does not (`classAliases`). Dates have no year, so an identical row a year later can be missed. |
| `D.M.` dates | A grade from the same calendar day of an earlier year counts as current in the relevance filter. |
| Plaintext credentials | Readable by any process running as the same user. |
| Unofficial WhatsApp protocol | `whatsmeow` can break on protocol changes; bans are possible. |
| Embedded OAuth client | Removing the legacy client breaks older installs at once. |
| SQLite on NFS/FUSE | WAL locking is unreliable there. |
