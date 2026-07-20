# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

For a full tour of components, data flow, and design trade-offs, read **`ARCHITECTURE.md`**. This file only documents the things not easily discovered from the code.

---

## Package layout

All sub-packages live under `internal/` (enforced by the Go toolchain — nothing outside this module can import them):

| Package | Role |
|---|---|
| `internal/msgtypes` | Canonical domain event: `Message` struct + `EventCode` enum. No deps. |
| `internal/fetch` | Raw HTTP client for e-Dnevnik (SAML/SSO auth, cookie jar). |
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

- **Go 1.26+ is mandatory** — the code calls `sync.WaitGroup.Go` (Go 1.25) and `context.WithoutCancel` (Go 1.21). `go.mod` pins `go 1.26`; older local toolchains will trigger an auto-download via `GOTOOLCHAIN` or fail to build.
- Build system: [Task](https://taskfile.dev/) via `Taskfile.yml`. `CGO_ENABLED=0` is set at the taskfile level; do not override — the whole point of `modernc.org/sqlite` is a static binary.

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

The fan-out is a hand-rolled **non-blocking** dispatch (not `teivah/broadcast`, which was removed): for each message it does a `select { case ch <- g: default: storeOverflow(...) }` per messenger. A messenger that has fallen behind (full buffer) has the message spilled to its failed-message queue for next-cycle delivery, so a slow/stalled messenger (e.g. mail mid-retry) never paces the others. Trade-off: under sustained overload a slow messenger's messages are delivered a cycle late and slightly out of order.

### Dedup is single-threaded by design — `routines.go:msgDedup`

`wgFilter` spawns exactly one goroutine. This is not a scaling limitation to "fix" — it guarantees consistent first-run detection and avoids sqlite write contention against the messenger queue writes. `gradesMsg` is closed in a `defer` so the fan-out loop unblocks on ctx cancel.

### First-run seeding is silent on purpose — `internal/sqlitedb/db.go` + `msgDedup`

A fresh DB (`!eDB.Existing()`) causes `msgDedup` to store hashes but forward nothing. This prevents flooding on first install. **If a user deletes `.e-dnevnik.db.sqlite`, the next run silently re-seeds without alerts** — users typically interpret this as "the bot missed events". Preserve the first-run seed behaviour; any change here is a UX regression.

### TTL-based dedup re-fires after ~1 year — `internal/sqlitedb/db.go:CheckAndFlagTTL`

`DefaultEntryTTL = 9000h`. Expired rows are treated as absent and re-inserted. Long-lived installs will re-alert on stale events. Do not shorten this TTL without coordinating with the relevance-period filter in `msgDedup`.

### Bounded version check — `routines.go:versionCheck`

`versionCheckTimeout = 30s`. A stalled GitHub Releases endpoint must not hold the goroutine past the poll interval. When modifying `versionCheck`, keep the timeout in place.

### Bounded shutdown of background goroutines — `main.go`

Long-running background goroutines (the systemd watchdog) are tracked in a dedicated `bgWG` — **not** `wgMsg`/`wgScrape`/`wgFilter`/`wgVersion`. Shutdown awaits `bgWG` with a ceiling of `exitDelay = 10s`. New background goroutines started outside a poll cycle belong in `bgWG`.

### `math/rand/v2` continuous jitter — `main.go:durationRandJitter`

Factor drawn from a continuous `[0.9, 1.1)` distribution via `rand.Float64()`. Do not replace with a discrete-step variant — concurrent daemons would alias on a small number of wake times.

### Messenger implementation contract — `internal/messenger/*.go`

Every messenger follows an identical lifecycle and set of rules. When adding a new messenger:

1. **Exported entry point** signature: `func Name(ctx context.Context, eDB *sqlitedb.Edb, ch <-chan msgtypes.Message, cfg NameConfig) error`. Per-messenger credentials, recipient lists and `Retries` are carried in a dedicated `NameConfig` struct (e.g. `DiscordConfig`, `MailConfig`) defined alongside the entry point — keep the signature at four parameters rather than threading individual credential args.
2. **Permanent vs transient errors**: each messenger has a `markNamePermanent(err) error` function. Permanent errors (e.g. invalid token, 4xx that will never succeed) are wrapped with `retry.Unrecoverable(err)` to short-circuit `retry-go`; transient errors (timeout, 429) are returned unwrapped so retry fires.
3. **Partial delivery — `SkipRecipients`**: when a message is sent to a subset of recipients successfully, the successful IDs are added to `g.SkipRecipients`. On retry, iterate over recipients and skip those already in the set. Before requeing, call `mergeSkipRecipients` (deduplicated) so the list doesn't grow unboundedly across cycles.
4. **Queue writes must use `queueStoreCtx`** (see Shutdown-tolerant queue writes above) — never the raw `ctx`.

---

## Runtime considerations

- **WhatsApp first run** requires interactive pairing (QR code or phone PIN via `mdp/qrterminal`). Session stored in `.e-dnevnik.wa.sqlite`. Cannot be automated.
- **Google Calendar first run** launches a local HTTP server on `:9080` and opens a browser for OAuth2 consent. Token persisted to `calendar_token.json` (0600 via `google/renameio`).
- **Poll interval floor**: `tickInterval` is clamped to `DefaultTickInterval = 1h` in `flags.go`. Requests for shorter intervals are silently upgraded — do not remove this clamp (it protects the portal).
- **GOMEMLIMIT**: `automemlimit` auto-tunes to 90% of cgroup/system memory at startup. Container memory limits are respected without extra config.
- **Per-messenger rate limits** (`go.uber.org/ratelimit`, per-minute/hour): constants live as `<Name>APILimit` / `<Name>Window` in each messenger file. Changes to these values cascade to `<Name>MinDelay`.

## Config

TOML (`.e-dnevnik.toml`). Multiple `[[user]]` blocks supported. Each messenger section is independently optional — absence disables that messenger. Validation is fail-fast in `internal/config/validators.go`. The app does **not** enforce 0600 permissions on the config file; mentioning this is an operator responsibility is a known trade-off (see ARCHITECTURE.md §10).

## Flag variables

`parseFlags()` in `flags.go` stores all CLI flag results as **package-level pointer variables** (`*bool`, `*string`, `*time.Duration`, `*uint`). Code throughout the `main` package dereferences them directly — e.g. `*readingList`, `*relevancePeriod`, `*retries`, `*emulation`, `*daemon`. When adding a feature that must respect a CLI flag, add the var to `flags.go` and dereference it where needed; do not thread it through function arguments.

## Linting

`.golangci.yml` enables nearly everything. Disabled: `cyclop`, `funlen`, `mnd`, `varnamelen`, `wrapcheck`. Test files are excluded from most checks. `nilaway` is run separately via `task lint-nil`.

## Build-time ldflags

The build injects four `main` vars: `GitTag`, `GitCommit`, `GitDirty`, `BuildTime`. `versionCheck` skips the update ping if `GitTag == ""` or `GitDirty != ""` — i.e. local source builds don't hit GitHub.
