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
| `internal/messenger` | Eight messenger backends (Discord/Telegram/Slack/Mail/Calendar/CalDAV/WhatsApp/Webhook). |
| `internal/format` | Plain/HTML/Markdown formatters consumed by messengers. |
| `internal/oauth` | Google Calendar OAuth2 interactive flow (local HTTP server). |
| `internal/config` | TOML config load + validation. |
| `internal/logger` | Global `zerolog` wrapper. |
| `internal/version` | Reads dependency version from binary build info. |

Root-level files in the `main` package:

| File | Responsibility |
|---|---|
| `main.go` | Entry point, polling ticker loop, goroutine lifecycle, pprof profiling. |
| `routines.go` | `scrapers`, `msgDedup`, `msgSend` — the three pipeline stages — plus `versionCheck`. |
| `init.go` | Interactive first-run setup for WhatsApp (pairing) and Calendar (OAuth2). |
| `flags.go` | All CLI flags via `peterbourgon/ff/v4`. Flag vars are **package-level pointers** (see below). |
| `db.go` | `openDB` / `closeDB` helpers. |
| `log.go` | `initLog` — wires log level and colorized/JSON output from flag vars. |
| `quiet.go` | `quietWindow` (the `-q` flag value), `nextPollDelay` and `pollRuns`. |
| `seed.go` | Per-user first-run markers: `loadSeeded`, `markSeeded`. |
| `digest.go` | Weekly exam digest and `userSet`, the per-cycle record of failed and drifted users. |
| `drift.go` | Two-cycle confirmation and weekly alert for portal drift (`sendDriftAlerts`). |

---

## Toolchain

- **Go 1.27+ is mandatory** — `go.mod` pins `go 1.27`; older local toolchains will trigger an auto-download via `GOTOOLCHAIN` or fail to build. The source itself needs at least Go 1.26 (`errors.AsType`, `new(expr)`, both used throughout).
- Build system: [Task](https://taskfile.dev/) via `Taskfile.yml`. `CGO_ENABLED=0` is set at the taskfile level; do not override — the whole point of `modernc.org/sqlite` is a static binary.
- `enetx/surf` links the HTTP/3 stack and `enetx/g`'s generic instantiations unconditionally — it has no build tags — which is ~15 MB of the binary. Weigh that before adding anything else in that family.

## Commands

```bash
task build          # fmt → static build with ldflags (CGO off)
task build-debug    # fmt → race build (CGO on). Slower. Use for races only.
task test           # go test ./...
task lint           # fmt → golangci-lint (5m timeout)
task lint-nil       # fmt → nilaway. Separate pass, NOT part of `task lint`.
task fmt            # go mod tidy + gci + gofumpt + go fix + betteralign -apply
task modernize      # apply gopls/modernize fixes across the tree
task update         # go get -u + realign modernc.org/libc + go mod tidy (see below)
task update-major   # list available major-version upgrades (gomajor)
task update-tools   # install gci, gofumpt, betteralign (required for `task fmt`)
task tools          # install whichever of the three formatters is missing
```

Single test: `go test -run TestName ./path/to/package/`.

**`task update` currently breaks the build.** `go get -u` raises `quic-go` to v0.65.0, which removed an API that `enetx/http3` v1.0.9 still calls (`qlog.DefaultConnectionTracerWithSchemas`). Afterwards run `go get github.com/quic-go/quic-go@v0.64.1`, until an `enetx/http3` release builds against the newer version.

**Keep `enetx/surf` at v1.0.207 (with `enetx/http` v1.0.29).** From v1.0.224 surf depends on `enetx/quic-go`, which (through v1.0.18) does not compile for 32-bit ARM: `client_handshake.go` passes `quicvarint.Max` to `%d`, overflowing `int`. Tests pass, so only goreleaser's linux armv6/armv7 builds catch it — v0.46.4 failed to release that way. After `task update`, also run `go get github.com/enetx/surf@v1.0.207 github.com/enetx/http@v1.0.29`, and before bumping surf check `GOOS=linux GOARCH=arm GOARM=7 go build .`.

The main binary accepts `-t`/`--test` for an **emulation mode** that pushes a synthetic message through the full pipeline without scraping — use this to verify messenger credentials and formatting without waiting for real events.

`-0` / `--fulldebug` logs every scraped event before the dedup filter — the fastest way to debug "why didn't this alert fire?" questions. Implies `-v`.

`-c <file>` / `-m <file>` write CPU and heap pprof profiles. No PGO profile is committed: at about 10 ms of CPU per hourly cycle there is nothing for one to speed up. `go build` would pick up a `default.pgo` in the repo root on its own, so adding one needs no build change.

## Mandatory before every commit

`task fmt` must pass. After `go mod tidy` it runs four rewriting steps; the three external tools **must be installed** (`task update-tools`):

1. `gci` — import ordering
2. `gofumpt` — stricter gofmt
3. `go fix ./...` — toolchain modernisation rewrites (ships with Go, nothing to install). Expect it to touch code you did not edit after a Go upgrade.
4. `betteralign` — struct field alignment

`betteralign -apply` rewrites struct field order to minimise padding. It will reorder fields in types you touch — this is expected, do not revert it.

## Keep the docs in step

A change to behaviour, CLI flags, config keys, messengers, packages, tasks or anything these files state must update them **in the same commit**:

- `README.md`: user-facing behaviour, flags and configuration
- `CLAUDE.md`: load-bearing invariants and things not discoverable from the code
- `AGENTS.md`: the compact quick reference
- `ARCHITECTURE.md`: components, data flow, persistence and trade-offs
- `.e-dnevnik.toml.example`: every config key, commented, with correct guidance (it ships in the release archives)

Check them against the code rather than from memory: a renamed identifier, a changed limit or a new backend makes each of them wrong at once.

---

## Load-bearing invariants

These behaviours are not enforced by the type system or tests. Breaking them manifests as data loss, duplicate alerts, or shutdown hangs that only appear in production.

### Shutdown-tolerant queue writes — `internal/queue/queue.go:StoreFailedMsgs`

`StoreFailedMsgs` swaps a cancelled context for a detached one (`detachedCtx`: `context.WithoutCancel` + `storeTimeout = 5s`). The purpose: when the main context is already cancelled (SIGTERM arrived mid-send), the final sqlite write to the failed-message queue **must still complete**, otherwise the message is lost forever.

It lives inside the store, not at the call sites, so no send path can forget it. Do not move it back out, and do not add a raw variant: every queue write persists a message dedup has already flagged. Messengers call it through `storeQueued` (`internal/messenger/common.go`), which also logs the failure.

### Outbox: flagged events survive a hard kill — `routines.go`, `internal/queue/queue.go`

Dedup never re-fires a flagged event, so a flag that commits before delivery is an alert SIGKILL, an OOM kill or a power cut can lose. `CheckAndFlagOutbox` therefore writes each forwarded event's outbox row **in the same transaction as its flag**, deciding `forward` inside the callback; don't flag and write separately. `runPollCycle` clears the outbox after the cycle, unless a queue write failed that cycle (the row may be the last copy) or the cycle was held for quiet hours, and `recoverOutbox` moves leftovers to every enabled messenger's queue at the start of the next. Accepted costs: nothing records which messengers already delivered a row, so a kill can duplicate; and the digest's row is not in its claim's transaction, because `FetchAndStore` has no second row to write. `enabledQueues` must stay in step with `msgSend`. Read the outbox only through `FetchOutbox`: a failed read counts as a failed queue write, so `ClearOutbox` keeps rows that were never moved.

### Two-level WaitGroup in `msgSend` — `routines.go`

`msgSend` uses a dedicated `wgInner` to track the per-messenger goroutines. The deferred sequence closes **every messenger channel** first, **then** `wgInner.Wait()`. Reversed ordering deadlocks because each messenger's `range` loop only exits once its channel is closed.

A slow messenger still holds the *cycle*, since `runPollCycle` waits for all of them, so its time is bounded too: `stopsCycle` for failures, `MailCycleLimit` for mail's rate limiter, and `mailTempError` for a mail server answering 4xx (the first message spends the retry delays, later 4xx answers that cycle get one attempt each, while connection failures keep their retries).

The fan-out is a hand-rolled **non-blocking** dispatch (not `teivah/broadcast`, which was removed): for each message it calls `dispatch()` per messenger, which does `select { case ch <- g: default: storeOverflow(...) }`. A messenger that has fallen behind (full buffer) has the message spilled to its failed-message queue for next-cycle delivery, so a slow/stalled messenger (e.g. mail mid-retry) never paces the others. Trade-off: under sustained overload a slow messenger's messages are delivered a cycle late and slightly out of order.

### Dedup is single-threaded by design — `routines.go:msgDedup`

`wgFilter` spawns exactly one goroutine. This is not a scaling limitation to "fix" — it guarantees consistent first-run detection and avoids sqlite write contention against the messenger queue writes. `gradesMsg` is closed in a `defer` so the fan-out loop unblocks on ctx cancel.

### First-run seeding is silent on purpose, per user — `seed.go` + `msgDedup`

A user's first run stores hashes but forwards nothing. This prevents flooding on first install. **If a user deletes `.e-dnevnik.db.sqlite`, the next run silently re-seeds without alerts** — users typically interpret this as "the bot missed events". Preserve the first-run seed behaviour; any change here is a UX regression.

"First run" is decided **per user**, through a `seeded\x00<user>` marker that `markSeeded` writes only after that user's scrape succeeded (not in the cycle's `failed` set, ctx not cancelled). A login that finds no active classes returns `scrape.ErrNoActiveClasses`, which joins that set without failing the run: seeding on nothing would alert the user's history next cycle. It used to be decided for the whole database ("does any row exist"), so a first run that scraped user A but not B left B unseeded and the next cycle alerted B's entire history; adding a `[[user]]` to an existing install did the same. A database from before the markers (rows, but no `seeded-migrated` key) is migrated once by `loadSeeded`: every configured user is marked seeded, because the alternative is losing a cycle of alerts on upgrade. Accepted cost: a `[[user]]` added in that same upgrade alerts its history once. `msgDedup` with a nil `seedRun` keeps the old whole-database rule; only tests pass nil.

### TTL-based dedup re-fires after ~1 year — `internal/sqlitedb/db.go:CheckAndFlagTTL`

`DefaultEntryTTL = 9000h`. Expired rows are treated as absent and re-inserted. Long-lived installs will re-alert on stale events. Do not shorten this TTL without coordinating with the relevance-period filter in `msgDedup`.

### `Message.Fields` is the dedup identity — `internal/scrape/helpers.go:cellValues`

`msgDedup` hashes `(Username, Subject, Fields)`, so `Fields` is identity, not presentation: **any change to which cells `cellValues` emits re-alerts every stored event once**, bounded only by the relevance period. Moving to one entry per `div.cell` did that, and folded the note column into the identity — a teacher editing a note now re-alerts an old grade. Intended, but user-visible: ship such a change with a release note.

**Known limitation — year-less identity.** Grade dates are `D.M.` with no year, and a single-class student's grade `Subject` carries no class name (only multi-class users get ` / <class>`; final grades always do). So the identical row — same subject, day, month, grade and note — in the next school year hashes to a key still inside `DefaultEntryTTL` (~375 days) and is never alerted. Both fixes cost more than the rare miss: adding the school year to the identity re-alerts every stored event once, and a TTL under a year is unsafe because a reading can stay on the portal for the whole school year. Revisit only together with an identity change that ships a release note anyway.

**Class count in identity.** A `Subject` gains ` / <class>` only while a user has more than one active class (`multiClass`, `internal/scrape/scrape.go`), so gaining a second class or finishing one renames every subject. `classAliases` (`routines.go`) keeps that from re-alerting: `Message.Class` names the count-dependent suffix and `CheckAndFlagOutbox` counts the other form as seen. Final grades always carry the suffix, so they deliberately leave `Class` empty. Don't replace this with always including the class: that re-alerts every single-class user once and changes the frozen calendar event IDs. `examEventOf` still hashes `Subject`, but an aliased exam is never forwarded, so calendars are untouched. Accepted costs: a row in a newly added class identical to one stored before the change (subject, date, grade, note) counts as seen, and since grade dates have no year that includes last school year's rows while they are inside `DefaultEntryTTL` (`-0` logs each such hit), and an exam whose note is edited after the change gets a second calendar event.

`Message.Average` (the subject's grade mean) is the counter-example: display only. `msgDedup` takes the key *before* `withAverage` appends it as a `Prosjek` row, since it moves with every new grade.

`cellValues` is the single reader for grades, national exams and readings. Keep it that way — these were once three separate `div.cell > span` reads, and fixing only the grades one left the others dropping span-less cells and shifting later values a column left. The final-grade row is deliberately off this path: a single label/value row that compacts empty cells on purpose.

Course pages wrap tables in `div.tab-content` (observed: one, `.active`, with `data-schoolyear`); `/grade/all` has no wrapper. Reaching through it needs a descendant combinator, which would also reach inactive years, so every table reader is gated on `tabScope.includes`. It fails open on both no tabs and no `.active` — a silent empty scrape reads as a quiet school day, which is worse than a duplicate.

### Weekly exam digest — `digest.go`, `routines.go:msgDedup`

`msgDedup` collects every scraped `Exam` per user **before** the dedup check, and after the scrape channel closes `sendDigests` emits one `ExamDigest` per user. The digest skips both dedup and the first-run seed by design: it's a schedule, not an event. It is **on by default**: `Digest.Enabled` is a `*bool` so a missing key means on and only `enabled = false` turns it off. Runtime code reads the derived `Digest.Active`, never `Enabled`.

- **At most once per week:** `claimDigestWeek` records the covered Monday under `digest-week\x00<user>` through `FetchAndStore`, *before* the blocking handoff. The handoff can't fail, but a claim written after it could, and then the digest would repeat every hourly cycle of the window.
- **A failed scrape is not a quiet week:** `scrapers` records each user whose scrape returned an error, `scrape.ErrNoActiveClasses` included, in a per-cycle `userSet`. Those users are skipped *unclaimed* and retried on the next cycle. Don't drop that check: a portal outage would otherwise claim the week with no exams.
- **No send window:** `digestWeek` returns the week after the *latest past* `day@hour`, so any later poll catches up and a late digest lists from today. A window ending at Monday was tried and lost weeks whenever polls straddled it (`hour = 23` with ±10% jitter, or `-i` longer than the window).
- **Local time is `TZ`:** `main.go` embeds `time/tzdata` because the Alpine image ships none, and Go would otherwise fall back to UTC without any error. The import alone is not enough: since Go 1.21 the next package to initialise is the first by import path whose dependencies are done, so `github.com/BurntSushi/toml/internal` (imports only `time`) runs before `time/tzdata`, and its `localOffset` calls `time.Now().Zone()`, which settles `time.Local` to UTC for good (0.46.2 ran on UTC in Docker that way; the linux binary's `go:main.inittasks` shows it, #20 vs #28). `initTimeZone` therefore loads `TZ` again at the top of `main`; keep it before anything starts a goroutine. A host with zoneinfo cannot reproduce the bug, so verify in the Alpine image.
- Calendar backends receive the digest through the fan-out but drop it: `examEventOf` and `QueueAccepts` both reject non-`Exam` codes. For the same reason `checkDigestConf` leaves `Digest.Active` false when no chat messenger is enabled, so it must run after the messenger checks in `LoadConfig`.

### Portal drift detection — `internal/scrape/drift.go`, `drift.go`

A selector that stops matching yields an empty scrape (`logEmptyResult`), but a scrape that still finds rows and reads them wrong looks healthy. `checkDrift` cross-checks each scrape against the portal itself: every grade table must lead with `Datum` and carry `Ocjena` (`isStaleEvent` reads `Fields[0]` as the date), and the mean of the scraped grades must equal the course list's `div.list-average-grade`, an independent server-side figure (verified 26/26 on the live portal, 2026-11). Keep `gradeSum` counting the same cells as the portal.

`GetGradesAndEvents` returns a `*DriftError` only after an otherwise complete scrape, and `scrapers` records it with `userSet.addDrift`, **not** as a failure: the events went out, so seeding and the digest must proceed. `sendDriftAlerts` then needs the finding on **two consecutive** completed scrapes (`portal-drift\x00<user>`: a grade added between the two fetches looks like drift for one cycle). A failed scrape keeps the state. Once confirmed, it fails the run every cycle but alerts the messengers at most once a week, as a `PortalDrift` event that bypasses dedup like the digest. A clean scrape disarms the check but keeps that week's alert on record (`clean:week:<monday>`), so a flapping portal still alerts once. If the course list loses its averages the check goes blind; that only warns in the log. It is opt-in: `--driftcheck` (`*driftCheck`, default off) sets `scrape.CheckDrift` and gates both `scrapers` and `sendDriftAlerts`, so off the check neither runs nor records state. Subjects whose name appears twice on either page are skipped, and the tolerance (0.01) allows for the portal truncating rather than rounding.

### Bounded version check — `routines.go:versionCheck`

`versionCheckTimeout = 30s`. A stalled GitHub Releases endpoint must not hold the goroutine past the poll interval. When modifying `versionCheck`, keep the timeout in place.

### Bounded shutdown of background goroutines — `main.go`

Long-running background goroutines (the systemd watchdog) are tracked in a dedicated `bgWG` — **not** `wgMsg`/`wgScrape`/`wgFilter`/`wgVersion`. Shutdown awaits `bgWG` with a ceiling of `exitDelay = 10s`. New background goroutines started outside a poll cycle belong in `bgWG`.

### `math/rand/v2` continuous jitter — `main.go:durationRandJitter`

Factor drawn from a continuous `[0.9, 1.1)` distribution via `rand.Float64()`. Do not replace with a discrete-step variant — concurrent daemons would alias on a small number of wake times.

### Browser impersonation contract — `internal/fetch/chrome.go`

**`SecureTLS()` in `newSurfClient` is mandatory.** surf skips certificate verification by default, and because the ClientHello is identical either way, no fingerprint or wire test can notice. `TestClientRejectsUntrustedCertificate` is the only guard: without the call, anyone on the network path collects students' passwords.

The portal sits behind F5 BIG-IP, so `internal/fetch` impersonates Chrome 152 via `enetx/surf`, and **one profile owns every browser-identifying signal**: user agent, client hints, per-method header order, the TLS ClientHello behind JA3/JA4, and the HTTP/2 SETTINGS. Never hand-set any of them. They are only coherent because they come from a single source, and a value contradicting the fingerprint beneath it is a louder signal than sending nothing at all.

What the profile cannot know about *this* portal is corrected in `chromeNavigationMW`, registered at `overrideMWPriority = 999`. surf runs middleware **lowest priority first** and its own header pipeline registers at 0, so a lower number here loses every override silently. The rules living there:

- **`Accept-Language` stays `AcceptLanguageHR`.** surf defaults to en-US; the login alert matcher and the subject names both depend on Croatian responses.
- **`Priority` is deleted on GET *and* POST.** It is an HTTP/2 header that surf inserts unconditionally, and this portal is Apache negotiating no ALPN at all — real Chrome cannot send it on this connection.
- **`Connection: keep-alive` is prepended to `HeaderOrderKey`.** surf's order map has no slot for it, so it otherwise lands last rather than directly after `Host`.
- **The login POST is a navigation, not an XHR.** surf models POSTs as XHR (`Accept: */*`, `Sec-Fetch-Mode: cors`); a form submit carries the navigation `Accept` and an `Origin`.
- **`Cache-Control: max-age=0` is *set* on the POST, and `Pragma` deleted.** Chrome sends `max-age=0` on reload-like navigations — address bar, reload and form submit — and never `Pragma`. A link click carries `Sec-Fetch-User: ?1` *without* `Cache-Control`, which is what rules out user activation as the trigger and is why our plain GETs correctly omit it. The 302's GET inherits it from the POST, so `cache-control` is absent from `postOnlyHeaderOrder`. surf v1.0.224 (not yet usable, see Commands) inserts no `Cache-Control` on a POST, and `HeaderOrderKey` lists only headers present when surf's pipeline runs, so `chromeNavigationMW` slots `cache-control` into the order itself; without that it lands last.
- **The POST order needs three corrections** (`chromePostClientHints`, then `content-type` before `user-agent`, then `origin` before `accept`). surf's POST profile splits the client hints across three slots and inverts those two pairs. Both order lists live in `wire_test.go` as capture constants.

`chromeNavigationMW` runs **once per `Do()`, not once per redirect hop** — surf applies middleware above its own `retry:` label, and redirects are followed inside `cli.Do`. The redirect handler then copies the initial request's headers onto every hop, stripping only the `Content-*` group on a POST→GET rewrite. So the POST rules above leak forward, and per-hop corrections need `CheckRedirect`, which runs *after* that copier:

- **`Origin` is deleted on a redirect hop that became a GET** (`fixupRedirectHeaders`). A Chrome 152 capture of this portal shows no `Origin` on a GET navigation, so the login POST's would contradict the profile on the post-login request. `Sec-Fetch-User` and `Upgrade-Insecure-Requests` are deliberately *kept*: the same capture shows Chrome sending both on navigations. A method-preserving redirect (307/308) is still a form submit and keeps its `Origin`.
- **Wrap surf's `CheckRedirect`, never replace it.** It carries the max-redirect ceiling and the same-host rule; the builder's `checkRedirect` option discards both (`TestRedirectCeilingSurvivesOriginStrip` follows 129 hops instead of 10 if you do).

Still unresolved: the redirect GET carries surf's **POST** `HeaderOrderKey` list. DevTools displays request headers alphabetically sorted, so a DevTools capture cannot establish Chrome's wire order — settling it needs the Network panel's "view source" or a raw wire capture.

**Any request with a body must set `GetBody`** (see `doSAMLRequest`). surf sets `Body` but never `GetBody`, and because the Chrome ClientHello offers h2 while the portal negotiates none, surf retries over HTTP/1.1 — which only works for a request it can rewind. Bodyless GETs replay regardless, so omitting it breaks authentication outright while leaving unauthenticated fetches working, and no GET-only test will catch it.

Verify header changes by diffing against a real capture, never from memory. The `Priority`, `Connection` and redirect-`Origin` rules above were all found that way, and two plausible shortcuts were wrong: matching our *own* GET order (Chrome orders a redirect hop differently from a fresh navigation) and deleting the `Content-Length` header (the ordered writer re-adds it from the order list).

**DevTools is not sufficient.** The Network panel sorts request headers alphabetically and CDP returns a map, so neither shows wire order; Go's `Header` map canonicalises, so neither shows casing. Order and casing exist only on the socket.

**Capture recipe.** `TestRedirectHopMatchesChromeWireOrder` (`internal/fetch/wire_test.go`) carries a raw HTTP/1.1 listener that records header names in wire order, and drives the real transport — not a stub — through a POST→302→GET. To re-derive the expected sequence after a surf bump:

1. Point that listener's address at a browser instead of the client: serve a form whose action POSTs back, then 302 to another path. `127.0.0.1` is a trustworthy origin, so Chrome sends the full set (client hints included) over plain HTTP — no certificate needed.
2. Drive the form in Chrome, read the redirect hop's names off the listener, and update `chromeRedirectWireOrder` from that capture.

Never edit `chromeRedirectWireOrder` to match a failing test: a change there means either surf's profile moved or Chrome did, and only a capture distinguishes them.

For everything else, tests reach a stub via `newStubClient`, which swaps `GetClient().Transport` so surf's middleware still runs.

**Header-name casing cannot be fixed from here.** Chrome sends the `sec-ch-*` client hints lowercase and everything else canonicalised; surf's own constants are lowercase (`header.SEC_CH_UA = "sec-ch-ua"`), but `enetx/http` writes every name through `CanonicalHeaderKey` (`header.go`), so they reach the wire as `Sec-Ch-Ua`. That is an upstream gap, not something `chromeNavigationMW` can correct — the wire-order test compares case-insensitively for that reason.

### Messenger implementation contract — `internal/messenger/*.go`

Every messenger follows an identical lifecycle and set of rules. When adding a new messenger:

1. **Exported entry point** signature: `func Name(ctx context.Context, eDB *sqlitedb.Edb, ch <-chan msgtypes.Message, cfg NameConfig) (err error)`. The return is **named** so the panic guard (below) can set it. Per-messenger credentials, recipient lists and `Retries` are carried in a dedicated `NameConfig` struct (e.g. `DiscordConfig`, `MailConfig`) — keep the signature at four parameters rather than threading individual credential args.
2. **Panic guard — `recoverMessenger`**: install `defer func() { if r := recover(); r != nil { err = recoverMessenger(ctx, eDB, NameQueueName, ch, r, inflight) } }()` at the top. An unrecovered send-path panic would crash the whole process (taking every other messenger down); the guard instead requeues the **in-flight** message, drains the rest of `ch` to the queue, and returns `ErrMessengerPanic`. Track `inflight *msgtypes.Message`: set `inflight = &g` immediately before processing a live message off `ch`, `inflight = nil` after — and leave it nil on the queued-resend path (that row is still in the DB; re-storing it would duplicate). **Never hold a mutex across panic-able code under this guard** — the recover does not unlock it, and a leaked lock hangs the next cycle at `Lock` (see `ensureCalendarInit`, which `defer`s its unlock for exactly this reason).
3. **Permanent vs transient errors + poison-drop**: each messenger has a `markNamePermanent(err) error`. Permanent errors (a 4xx that will never succeed for that recipient) are wrapped `retry.Unrecoverable(permanentError{err})` — the outer marker short-circuits `retry-go`, and the inner `permanentError` sentinel **survives `retry.Do`'s marker stripping** (retry-go v5 strips the outer `Unrecoverable` on return) so `isPermanentSendErr` can still detect permanence *after* the retry loop. Transient errors (timeout, 429) are returned unwrapped so retry fires. A permanent per-recipient failure is **poison-dropped**: logged loudly, recorded with `run.poison`, and **not** requeued (it would otherwise retry every cycle until `MaxQueueAge`); only transient failures set `anyFailed` and requeue. A third class, **config errors**, is marked with `markConfigErr` (`configError`, which like `permanentError` survives `retry.Do`): the retry loop stops but the message is queued, because the failure (a bad token, login, session or URL) applies to every recipient and dropping would lose already-flagged alerts until a restart. Record every non-permanent failure with `run.failed(err)`: it returns `true` for a config error, and the loop must `break` then. `run.finish` returns that flag as `processName`'s result, and the entry point's `cycleSender` then queues the rest of the cycle unsent, so a bad SMTP password is tried once per cycle, not once per message (account lockout). An unreachable server stops the cycle the same way once its retries are spent (`stopsCycle`: `isUnreachable`, plus mail's `mailConnError`), since each further message would cost minutes of retry delay while the whole poll cycle waits; those errors keep their retries, unlike config errors. Every messenger routes credential failures there: CalDAV (401/403/404/405/409, redirects), Slack (`slackAuthErrors`, which includes the workspace-wide `restricted_action`, and an HTTP-level 401/403), Telegram (`ErrorUnauthorized`, and `ErrorNotFound`, which is a malformed token in the URL), Discord (401; a 403 with Discord's JSON body stays per-user permanent, while any 4xx without that body came from an edge or proxy and is retried; and a 429 asking for longer than `DiscordMinDelay`, because discordgo sleeps out both its 429 retry and a bucket's reset with `time.Sleep`, ignoring the context, so `WithRetryOnRatelimit` stays false and `discordInit` hands each cycle fresh buckets), WhatsApp (`ErrNotLoggedIn`, `ErrClientIsNil`), Google Calendar (401, `oauth.IsAuthRejected` — `invalid_grant`/`invalid_client`/`unauthorized_client` from the oauth2 transport — any 403 except `calendarRateReasons`, which retry (an exhausted daily quota and lost write access land here), and a 404, which also forgets the cached calendar ID) and mail (`isMailAuthErr`, an invalid From, and `isMailAccountRejected`: a 5xx to MAIL FROM other than 503, which is go-mail's missing RSET after a failed DATA, or 530/534/535 to RCPT TO). Mail has no `markNamePermanent`: `sendMailBatch` classifies each attempt itself, and a 5xx to RCPT TO only drops the recipient when its enhanced status names them (`isPermanentMailSendErr`), because Postfix delays sender rejections to that command by default. Never classify an auth failure as permanent. Webhook is the exception to all of this: each endpoint has its own credentials, so nothing there stops the cycle or is poison-dropped; a non-2xx other than 5xx/408/429 is bare `retry.Unrecoverable` and an unreachable endpoint is skipped for the cycle (`errWebhookDown`), as is one that answers 5xx to `webhookDownStrikes` messages in a row (not one: a 500 can be that message's fault, and it is retried first every cycle). An error of **unknown reach** (Slack codes on neither list, a Slack HTTP 4xx other than 401/403 since channel errors arrive in a 200, a mail 5xx that neither names the recipient nor rejects the account, such as a 5.7.x to RCPT TO or any 5xx to DATA; accepted cost: mail content rejected for good comes back every cycle until `MaxQueueAge`, since quota and account blocks arrive the same way) gets a fourth treatment, bare `retry.Unrecoverable(err)`: it requeues that recipient only and the loop moves on — a config error would let one odd channel block every other for 30 days, a permanent one would drop the alert. Any classification that must survive `retry.Do` has to be decided per attempt inside `markNamePermanent` and carried as a marker type (`permanentError`, `configError`, `discordStaleChannelError`): `errors.As` on the aggregate returns the *first* attempt that matches, so inspecting a raw error type after the loop sees an earlier transient failure instead. The only post-loop readers are `isUnreachable`, mail's `mailConnError` check and the webhook's `errWebhookServer` check, and all go through `lastAttempt`. go-mail never clears `Msg.SendError` either, so `sendMailBatch` decides whether an attempt reached a message from that attempt's error, not from the messages. Slack API errors arrive as the *value* type `slack.SlackErrorResponse`; match it by value.
4. **Partial delivery — `SkipRecipients`**: successful *and* poisoned recipient IDs are merged into `g.SkipRecipients` before requeuing (via `mergeSkipRecipients`, deduplicated so it doesn't grow unboundedly). On retry, iterate over recipients and skip those already in the set.
5. **`resendQueued` drops what has expired** (`queuedExpired`): an exam already sat, or a digest or drift warning whose week has ended. Grades never expire; a late grade is still news.
6. **Queue writes go through `storeQueued`** (or `queueUndelivered` for a whole channel). Both are shutdown-tolerant already (see above); pass the raw `ctx`. `queue.StoreFailedMsgs` counts failed writes per database and queue, which is how `resendQueued` knows to keep the original row and `ClearOutbox` to keep the outbox.
7. **A send that did not land reaches the entry point's return.** `recipientRun` reports poison drops and failures through `recordSendFailure`, the calendar backends call it directly, and each entry point ends in `return sendFailureErr(eDB, NameQueueName)`. That is the only way a failed send reaches `exitWithError`, and so the exit status of `-t` and one-shot runs. A shutdown cutting a send short does not count.
8. **Every API client has a timeout.** The send context only ends on shutdown, so a server that accepts a request and never answers would otherwise hold the poll cycle for good, with the watchdog still reporting healthy. `slack.New` and `oauth2.NewClient` default to no timeout (`SlackTimeout`, `oauth.httpTimeout`).

### Outbound formatting contract — `internal/format`, `internal/messenger`

Two rules that were each established by sending a real message and reading it back, not by reasoning about the markup.

- **Slack honours no backslash escape. Never escape mrkdwn metacharacters.** `\*` reaches the reader as a literal backslash followed by an asterisk — confirmed on a live render, where every username carrying an underscore arrived as `pero\_peric`. `markupEscape` therefore escapes only what Slack does interpret: the `&`/`<`/`>` entities, which are required *everywhere* including inside a code fence. A backtick is substituted rather than escaped, because it cannot be escaped either and three in a row would close `MarkupMsg`'s fence. One escaper serves header and body — splitting them is how the two halves came to hold contradictory beliefs about the same renderer. Accepted cost: a literal `*` in a subject ends the header's bold early.
- **Truncate the input, never the rendered output** (`truncateRendered`). Cutting the finished string splits whatever it lands in: a `<b>`/`<pre>` tag Telegram's parser rejects, the closing ``` of a Slack fence, or an `&amp;` that arrives as `&am`. Dropping whole description/value pairs and re-rendering can only ever lose rows. `truncateWithEllipsis` is for unstructured text only — plain-text bodies, mail subjects, Discord field values — and using it on rendered markup is the bug this rule exists to prevent.

### Calendar event identity — `internal/messenger/exam.go`

`examEventOf` is the single source of which messages reach a calendar and of the event ID both calendar backends dedupe on (Google 409, CalDAV 412). The ID's input bytes are frozen; `TestExamEventOfGolden` and `TestProcessCalendarEventIDGolden` pin them. Changing them re-inserts every future exam already in users' calendars. CalDAV bodies are written by `ical.go`, not goics' encoder, which splits `\r\n` when a fold lands between them.

---

## Runtime considerations

- **WhatsApp first run** requires interactive pairing (QR code or phone PIN via `mdp/qrterminal`). Session stored in `.e-dnevnik.wa.sqlite`. Cannot be automated.
- **Google Calendar first run** launches a local HTTP server on `:9080`, or another port if it is busy, and opens a browser for OAuth2 consent with PKCE. Token persisted to `calendar_token.json` (0600 via `google/renameio`). 

  **Two OAuth clients are embedded, on purpose.** A refresh token works only with the client that issued it. New setups consent through the desktop client (`calendar_credentials.json`); it must stay a desktop one, since a web client needs an exact redirect, which breaks the port fallback. Installs from before it hold tokens from the web client (`calendar_credentials_legacy_web.json`), and `calendarOAuthConfig` keeps them on it: a token file with no `client_id` is theirs. `saveToken` records `client_id` on every write. Removing the legacy file or deleting that client in Google Cloud breaks every remaining old install at once, so announce a date in a release first.
- **Scrape budget**: each user's whole scrape gets `scrapeBudget`, at least 20 minutes whatever `-r` says, because it spans every request. A stalled portal can hold the poll cycle that long.
- **Poll interval floor**: `tickInterval` is clamped to `DefaultTickInterval = 1h` in `flags.go`. Requests for shorter intervals are silently upgraded — do not remove this clamp (it protects the portal).
- **Quiet hours**: `-q`/`--quiethours` (default `23:00-06:00`, `off` disables) keeps daemon polls out of a local-time window, one-shot and `-t` runs excepted. `nextPollDelay` re-arms a poll due inside the window to its end: re-arming a full interval and skipping on arrival never polled at all with `-i 24h` started inside the window. Its jitter stays within the active gap for the same reason. The first poll after startup always runs (`pollRuns`), to validate credentials, and the tick branch's skip remains only for clock changes. Inside the window that poll is **held** (`runPollCycle`'s `hold`, `holdAlerts`): a restart at night used to alert at once. It scrapes and flags, but starts no messengers, skips the digest and leaves the outbox for the next cycle's `recoverOutbox`, so held alerts arrive through the queues after the window. Accepted cost: messenger credentials are not exercised on a held poll (`-t` does that). The digest needs nothing extra because it already catches up on a later poll. Disabling is `off`, not an empty value: ff reads `--flag=` as a missing value and takes the next argument as the value.
- **GOMEMLIMIT**: `automemlimit` auto-tunes to 90% of cgroup/system memory at startup. Container memory limits are respected without extra config.
- **Per-messenger rate limits** (`go.uber.org/ratelimit`, per-minute/hour): constants live as `<Name>APILimit` / `<Name>Window` in each messenger file. Changes to these values cascade to `<Name>MinDelay`.

## Config

TOML (`.e-dnevnik.toml`). Each messenger section is independently optional — absence disables that messenger. `[[user]]` and `[[webhook]]` repeat. Validation is fail-fast: the `check*Conf` functions in `internal/config/config.go` and `digest.go`, with format predicates in `validators.go`. `LoadConfig` tightens the file to 0600 on every load (best-effort, warn-only on failure); `SaveConfig` writes 0600 atomically. Both SQLite stores get the same through `sqlitedb.PrepareFile` before they open, sidecars included: the dedup store holds queued grades, the WhatsApp one the device's session keys.

**Credentials are read once, at startup.** `cfg` is loaded in `main` and passed *by value* into every poll cycle, and nothing re-reads the file — `LoadConfigRaw` exists only for the two rewrite paths (`telegramPersistChatID`, `whatsAppPersistResolvedGroups`), which both go through `rewriteConfig` (`internal/messenger/common.go`). Rotating a token therefore requires a restart, and that is documented behaviour, not an oversight. The messengers' lazy clients are keyed on their credentials anyway (`credGuard`, `internal/messenger/creds.go`), so a rebuild happens if the inputs ever do start varying: `if cli == nil` alone would keep serving a client built from a superseded secret. Live rotation would additionally need a validating non-fatal loader (`LoadConfig` calls `logger.Fatal`, which must never run mid-cycle), would have to take `configRewriteMu` to avoid racing those rewrite paths, and could not cover WhatsApp or Calendar at all — their credentials live in `.e-dnevnik.wa.sqlite` and `calendar_token.json`, behind interactive pairing and OAuth.

## Flag variables

`parseFlags()` in `flags.go` stores all CLI flag results as **package-level pointer variables** (`*bool`, `*string`, `*time.Duration`, `*uint`). Code throughout the `main` package dereferences them directly — e.g. `*readingList`, `*relevancePeriod`, `*retries`, `*emulation`, `*daemon`. When adding a feature that must respect a CLI flag, add the var to `flags.go` and dereference it where needed; do not thread it through function arguments.

## Logging

Grades and teachers' notes are children's data. At Info and above, name an event by user, subject and code, never `%+v` of the message: `docker logs` and journald keep it. `-0` opts in to the content at Debug.

## Linting

`.golangci.yml` enables nearly everything. Disabled: `cyclop`, `depguard`, `dupl`, `exhaustruct`, `forbidigo`, `funlen`, `gochecknoglobals`, `gocognit`, `gomodguard`, `lll`, `mnd`, `noinlineerr`, `varnamelen`, `wrapcheck`, `wsl`. Test files are excluded from most checks. `nilaway` is run separately via `task lint-nil`.

## Build-time ldflags

The build injects four `main` vars: `GitTag`, `GitCommit`, `GitDirty`, `BuildTime`. `versionCheck` skips the update ping if `GitTag == ""` or `GitDirty != ""` — i.e. local source builds don't hit GitHub.
