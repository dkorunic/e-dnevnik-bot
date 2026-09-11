# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

For a full tour of components, data flow, and design trade-offs, read **`ARCHITECTURE.md`**. This file only documents the things not easily discovered from the code.

---

## Package layout

All sub-packages live under `internal/` (enforced by the Go toolchain — nothing outside this module can import them):

| Package | Role |
|---|---|
| `internal/msgtypes` | Canonical domain event: `Message` struct + `EventCode` enum. No deps. |
| `internal/fetch` | Chrome-impersonating HTTP client for e-Dnevnik (`enetx/surf`, SAML/SSO auth, cookie jar). |
| `internal/scrape` | Parses `fetch/` HTML into `msgtypes.Message` events. |
| `internal/sqlitedb` | SQLite KV dedup store. |
| `internal/codec` | CBOR (`fxamacker/cbor/v2`) encode/decode for `[]Message` queue persistence. |
| `internal/queue` | Dead-letter queue built on `sqlitedb` + `codec`. |
| `internal/messenger` | Six messenger backends (Discord/Telegram/Slack/Mail/Calendar/WhatsApp). |
| `internal/format` | Plain/HTML/Markdown formatters consumed by messengers. |
| `internal/oauth` | Google Calendar OAuth2 interactive flow (local HTTP server). |
| `internal/config` | TOML config load + validation. |
| `internal/logger` | Global `zerolog` wrapper. |
| `internal/version` | Reads dependency version from binary build info. |

Root-level files in the `main` package:

| File | Responsibility |
|---|---|
| `main.go` | Entry point, polling ticker loop, goroutine lifecycle, PGO/profiling. |
| `routines.go` | `scrapers`, `msgDedup`, `msgSend`, `versionCheck` — the three pipeline stages. |
| `init.go` | Interactive first-run setup for WhatsApp (pairing) and Calendar (OAuth2). |
| `flags.go` | All CLI flags via `peterbourgon/ff/v4`. Flag vars are **package-level pointers** (see below). |
| `db.go` | `openDB` / `closeDB` helpers. |
| `log.go` | `initLog` — wires log level and colorized/JSON output from flag vars. |

---

## Toolchain

- **Go 1.27+ is mandatory** — `go.mod` pins `go 1.27`; older local toolchains will trigger an auto-download via `GOTOOLCHAIN` or fail to build. The oldest language features actually used are `sync.WaitGroup.Go` (Go 1.25) and `context.WithoutCancel` (Go 1.21), so the pin — not the source — is what sets the floor.
- Build system: [Task](https://taskfile.dev/) via `Taskfile.yml`. `CGO_ENABLED=0` is set at the taskfile level; do not override — the whole point of `modernc.org/sqlite` is a static binary.
- `enetx/surf` links the HTTP/3 stack and `enetx/g`'s generic instantiations unconditionally — it has no build tags — which is ~15 MB of the binary. Weigh that before adding anything else in that family.

## Commands

```bash
task build          # fmt → static PGO build with ldflags (CGO off)
task build-debug    # fmt → race build (CGO on). Slower. Use for races only.
task test           # go test ./...
task lint           # fmt → golangci-lint (5m timeout)
task lint-nil       # fmt → nilaway. Separate pass, NOT part of `task lint`.
task fmt            # go mod tidy + gci + gofumpt + betteralign -apply
task modernize      # apply gopls/modernize fixes across the tree
task update         # go get -u + go mod tidy
task update-tools   # install gci, gofumpt, betteralign (required for `task fmt`)
task tools          # verify the three formatters are on PATH
```

Single test: `go test -run TestName ./path/to/package/`.

The main binary accepts `-t`/`--test` for an **emulation mode** that pushes a synthetic message through the full pipeline without scraping — use this to verify messenger credentials and formatting without waiting for real events.

`-0` / `--fulldebug` logs every scraped event before the dedup filter — the fastest way to debug "why didn't this alert fire?" questions. Implies `-v`.

`-c <file>` / `-m <file>` write CPU and heap pprof profiles. The production build uses `-pgo=auto`; the toolchain picks up `default.pgo` from the repo root automatically.

## Mandatory before every commit

`task fmt` must pass. It runs three tools, **all of which must be installed** (`task update-tools`):

1. `gci` — import ordering
2. `gofumpt` — stricter gofmt
3. `betteralign` — struct field alignment

`betteralign -apply` rewrites struct field order to minimise padding. It will reorder fields in types you touch — this is expected, do not revert it.

---

## Load-bearing invariants

These behaviours are not enforced by the type system or tests. Breaking them manifests as data loss, duplicate alerts, or shutdown hangs that only appear in production.

### Shutdown-tolerant queue writes — `internal/messenger/common.go`

Every messenger that stores un-delivered messages to the retry queue uses `queueStoreCtx(ctx)` — built on `context.WithoutCancel` + `storeTimeout = 5s`. The purpose: when the main context is already cancelled (SIGTERM arrived mid-send), the final sqlite write to the failed-message queue **must still complete**, otherwise the message is lost forever.

When adding a new messenger or touching send paths, the post-send `StoreFailedMsgs` call must use `queueStoreCtx`, not the raw `ctx`.

### Two-level WaitGroup in `msgSend` — `routines.go`

`msgSend` uses a dedicated `wgInner` to track the per-messenger goroutines. The deferred sequence closes **every messenger channel** first, **then** `wgInner.Wait()`. Reversed ordering deadlocks because each messenger's `range` loop only exits once its channel is closed.

The fan-out is a hand-rolled **non-blocking** dispatch (not `teivah/broadcast`, which was removed): for each message it calls `dispatch()` per messenger, which does `select { case ch <- g: default: storeOverflow(...) }`. A messenger that has fallen behind (full buffer) has the message spilled to its failed-message queue for next-cycle delivery, so a slow/stalled messenger (e.g. mail mid-retry) never paces the others. Trade-off: under sustained overload a slow messenger's messages are delivered a cycle late and slightly out of order.

### Dedup is single-threaded by design — `routines.go:msgDedup`

`wgFilter` spawns exactly one goroutine. This is not a scaling limitation to "fix" — it guarantees consistent first-run detection and avoids sqlite write contention against the messenger queue writes. `gradesMsg` is closed in a `defer` so the fan-out loop unblocks on ctx cancel.

### First-run seeding is silent on purpose — `internal/sqlitedb/db.go` + `msgDedup`

A fresh DB (`!eDB.Existing()`) causes `msgDedup` to store hashes but forward nothing. This prevents flooding on first install. **If a user deletes `.e-dnevnik.db.sqlite`, the next run silently re-seeds without alerts** — users typically interpret this as "the bot missed events". Preserve the first-run seed behaviour; any change here is a UX regression.

### TTL-based dedup re-fires after ~1 year — `internal/sqlitedb/db.go:CheckAndFlagTTL`

`DefaultEntryTTL = 9000h`. Expired rows are treated as absent and re-inserted. Long-lived installs will re-alert on stale events. Do not shorten this TTL without coordinating with the relevance-period filter in `msgDedup`.

### `Message.Fields` is the dedup identity — `internal/scrape/helpers.go:cellValues`

`msgDedup` hashes `(Username, Subject, Fields)`, so `Fields` is identity, not presentation: **any change to which cells `cellValues` emits re-alerts every stored event once**, bounded only by the relevance period. Moving to one entry per `div.cell` did that, and folded the note column into the identity — a teacher editing a note now re-alerts an old grade. Intended, but user-visible: ship such a change with a release note.

`cellValues` is the single reader for grades, national exams and readings. Keep it that way — these were once three separate `div.cell > span` reads, and fixing only the grades one left the others dropping span-less cells and shifting later values a column left. The final-grade row is deliberately off this path: a single label/value row that compacts empty cells on purpose.

Course pages wrap tables in `div.tab-content` (observed: one, `.active`, with `data-schoolyear`); `/grade/all` has no wrapper. Reaching through it needs a descendant combinator, which would also reach inactive years, so every table reader is gated on `tabScope.includes`. It fails open on both no tabs and no `.active` — a silent empty scrape reads as a quiet school day, which is worse than a duplicate.

### Bounded version check — `routines.go:versionCheck`

`versionCheckTimeout = 30s`. A stalled GitHub Releases endpoint must not hold the goroutine past the poll interval. When modifying `versionCheck`, keep the timeout in place.

### Bounded shutdown of background goroutines — `main.go`

Long-running background goroutines (the systemd watchdog) are tracked in a dedicated `bgWG` — **not** `wgMsg`/`wgScrape`/`wgFilter`/`wgVersion`. Shutdown awaits `bgWG` with a ceiling of `exitDelay = 10s`. New background goroutines started outside a poll cycle belong in `bgWG`.

### `math/rand/v2` continuous jitter — `main.go:durationRandJitter`

Factor drawn from a continuous `[0.9, 1.1)` distribution via `rand.Float64()`. Do not replace with a discrete-step variant — concurrent daemons would alias on a small number of wake times.

### Browser impersonation contract — `internal/fetch/chrome.go`

The portal sits behind F5 BIG-IP, so `internal/fetch` impersonates Chrome 152 via `enetx/surf`, and **one profile owns every browser-identifying signal**: user agent, client hints, per-method header order, the TLS ClientHello behind JA3/JA4, and the HTTP/2 SETTINGS. Never hand-set any of them. They are only coherent because they come from a single source, and a value contradicting the fingerprint beneath it is a louder signal than sending nothing at all.

What the profile cannot know about *this* portal is corrected in `chromeNavigationMW`, registered at `overrideMWPriority = 999`. surf runs middleware **lowest priority first** and its own header pipeline registers at 0, so a lower number here loses every override silently. The rules living there:

- **`Accept-Language` stays `AcceptLanguageHR`.** surf defaults to en-US; the login alert matcher and the subject names both depend on Croatian responses.
- **`Priority` is deleted on GET *and* POST.** It is an HTTP/2 header that surf inserts unconditionally, and this portal is Apache negotiating no ALPN at all — real Chrome cannot send it on this connection.
- **`Connection: keep-alive` is prepended to `HeaderOrderKey`.** surf's order map has no slot for it, so it otherwise lands last rather than directly after `Host`.
- **The login POST is a navigation, not an XHR.** surf models POSTs as XHR (`Accept: */*`, `Sec-Fetch-Mode: cors`, plus the no-cache pair); a form submit carries the navigation `Accept`, an `Origin`, and no cache-busting headers.

**Any request with a body must set `GetBody`** (see `doSAMLRequest`). surf sets `Body` but never `GetBody`, and because the Chrome ClientHello offers h2 while the portal negotiates none, surf retries over HTTP/1.1 — which only works for a request it can rewind. Bodyless GETs replay regardless, so omitting it breaks authentication outright while leaving unauthenticated fetches working, and no GET-only test will catch it.

Verify header changes by diffing against a real capture, never from memory: load the portal in Chrome, read the request over the DevTools protocol, and compare against `req.Write` output from a stub transport. The `Priority` and `Connection` rules above were both found that way. Tests reach the stub via `newStubClient`, which swaps `GetClient().Transport` so surf's middleware still runs.

### Messenger implementation contract — `internal/messenger/*.go`

Every messenger follows an identical lifecycle and set of rules. When adding a new messenger:

1. **Exported entry point** signature: `func Name(ctx context.Context, eDB *sqlitedb.Edb, ch <-chan msgtypes.Message, cfg NameConfig) (err error)`. The return is **named** so the panic guard (below) can set it. Per-messenger credentials, recipient lists and `Retries` are carried in a dedicated `NameConfig` struct (e.g. `DiscordConfig`, `MailConfig`) — keep the signature at four parameters rather than threading individual credential args.
2. **Panic guard — `recoverMessenger`**: install `defer func() { if r := recover(); r != nil { err = recoverMessenger(ctx, eDB, NameQueueName, ch, r, inflight) } }()` at the top. An unrecovered send-path panic would crash the whole process (taking every other messenger down); the guard instead requeues the **in-flight** message, drains the rest of `ch` to the queue, and returns `ErrMessengerPanic`. Track `inflight *msgtypes.Message`: set `inflight = &g` immediately before processing a live message off `ch`, `inflight = nil` after — and leave it nil on the queued-resend path (that row is still in the DB; re-storing it would duplicate). **Never hold a mutex across panic-able code under this guard** — the recover does not unlock it, and a leaked lock hangs the next cycle at `Lock` (see `ensureCalendarInit`, which `defer`s its unlock for exactly this reason).
3. **Permanent vs transient errors + poison-drop**: each messenger has a `markNamePermanent(err) error`. Permanent errors (invalid token, 4xx that will never succeed) are wrapped `retry.Unrecoverable(permanentError{err})` — the outer marker short-circuits `retry-go`, and the inner `permanentError` sentinel **survives `retry.Do`'s marker stripping** (retry-go v5 strips the outer `Unrecoverable` on return) so `isPermanentSendErr` can still detect permanence *after* the retry loop. Transient errors (timeout, 429) are returned unwrapped so retry fires. A permanent per-recipient failure is **poison-dropped**: logged loudly, added to `poisonedIDs`, and **not** requeued (it would otherwise retry every cycle until `MaxQueueAge`); only transient failures set `anyFailed` and requeue.
4. **Partial delivery — `SkipRecipients`**: successful *and* poisoned recipient IDs are merged into `g.SkipRecipients` before requeuing (via `mergeSkipRecipients`, deduplicated so it doesn't grow unboundedly). On retry, iterate over recipients and skip those already in the set.
5. **Queue writes must use `queueStoreCtx`** (see Shutdown-tolerant queue writes above) — never the raw `ctx`.

---

## Runtime considerations

- **WhatsApp first run** requires interactive pairing (QR code or phone PIN via `mdp/qrterminal`). Session stored in `.e-dnevnik.wa.sqlite`. Cannot be automated.
- **Google Calendar first run** launches a local HTTP server on `:9080` and opens a browser for OAuth2 consent. Token persisted to `calendar_token.json` (0600 via `google/renameio`).
- **Poll interval floor**: `tickInterval` is clamped to `DefaultTickInterval = 1h` in `flags.go`. Requests for shorter intervals are silently upgraded — do not remove this clamp (it protects the portal).
- **GOMEMLIMIT**: `automemlimit` auto-tunes to 90% of cgroup/system memory at startup. Container memory limits are respected without extra config.
- **Per-messenger rate limits** (`go.uber.org/ratelimit`, per-minute/hour): constants live as `<Name>APILimit` / `<Name>Window` in each messenger file. Changes to these values cascade to `<Name>MinDelay`.

## Config

TOML (`.e-dnevnik.toml`). Multiple `[[user]]` blocks supported. Each messenger section is independently optional — absence disables that messenger. Validation is fail-fast in `internal/config/validators.go`. `LoadConfig` tightens the file to 0600 on every load (best-effort, warn-only on failure); `SaveConfig` writes 0600 atomically.

## Flag variables

`parseFlags()` in `flags.go` stores all CLI flag results as **package-level pointer variables** (`*bool`, `*string`, `*time.Duration`, `*uint`). Code throughout the `main` package dereferences them directly — e.g. `*readingList`, `*relevancePeriod`, `*retries`, `*emulation`, `*daemon`. When adding a feature that must respect a CLI flag, add the var to `flags.go` and dereference it where needed; do not thread it through function arguments.

## Linting

`.golangci.yml` enables nearly everything. Disabled: `cyclop`, `funlen`, `mnd`, `varnamelen`, `wrapcheck`. Test files are excluded from most checks. `nilaway` is run separately via `task lint-nil`.

## Build-time ldflags

The build injects four `main` vars: `GitTag`, `GitCommit`, `GitDirty`, `BuildTime`. `versionCheck` skips the update ping if `GitTag == ""` or `GitDirty != ""` — i.e. local source builds don't hit GitHub.
