// SPDX-FileCopyrightText: 2025 Dinko Korunic
// SPDX-License-Identifier: MIT

package messenger

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/avast/retry-go/v5"
	"github.com/dkorunic/e-dnevnik-bot/internal/config"
	"github.com/dkorunic/e-dnevnik-bot/internal/format"
	"github.com/dkorunic/e-dnevnik-bot/internal/logger"
	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
	"github.com/dkorunic/e-dnevnik-bot/internal/sqlitedb"
	"github.com/dkorunic/e-dnevnik-bot/internal/version"
	"github.com/hako/durafmt"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/proto/waCompanionReg"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"go.uber.org/ratelimit"
	_ "modernc.org/sqlite" // register pure-Go sqlite database/sql driver
)

// ContextKey keeps context keys from colliding.
type ContextKey string

const (
	WhatsAppDBName                  = ".e-dnevnik.wa.sqlite"
	WhatsAppDBConnstring            = "file:%v?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(10000)&_txlock=immediate"
	WhatsAppDisplayName             = "Chrome (Linux)"
	WhatsAppOS                      = "Linux"
	WhatsAppAPILimit                = 10 // 10 req/min per user/IP
	WhatsAppWindow                  = 1 * time.Minute
	WhatsAppMinDelay                = WhatsAppWindow / WhatsAppAPILimit
	WhatsAppQueue                   = "whatsapp-queue"
	ConfFileKey          ContextKey = "confFile"

	// Bounds each SendPresence so a stalled socket can't block the callback.
	whatsAppPresenceTimeout = 5 * time.Second

	// How long resolved group JIDs are trusted. Re-resolving is what stops a
	// lost membership from stranding sends on a dead group, which would churn
	// the queue until MaxQueueAge.
	whatsAppGroupsCacheTTL = 24 * time.Hour

	// How long a resolution outlives lookups that match nothing. Long enough
	// to ride out an empty list after a reconnect, short enough that a group
	// the bot has left stops receiving sends.
	whatsAppGroupsStaleLimit = 2 * whatsAppGroupsCacheTTL
)

// SendPresenceBounded updates presence best-effort under its own timeout, so a
// stalled socket cannot block the caller. Errors are unrecoverable, so dropped.
func SendPresenceBounded(cli *whatsmeow.Client, p types.Presence) {
	ctx, cancel := context.WithTimeout(context.Background(), whatsAppPresenceTimeout)
	defer cancel()

	_ = cli.SendPresence(ctx, p)
}

var (
	ErrWhatsAppEmptyUserIDs   = errors.New("empty list of WhatsApp UserIDs (JIDs)")
	ErrWhatsAppUnableConnect  = errors.New("unable to connect to WhatsApp database")
	ErrWhatsAppUnableUpgrade  = errors.New("unable to upgrade WhatsApp database")
	ErrWhatsAppUnableDeviceID = errors.New("unable to get WhatsApp device ID")
	ErrWhatsAppUnableGroups   = errors.New("unable to list WhatsApp groups")
	ErrWhatsAppFailConnect    = errors.New("failed to connect to WhatsApp")
	ErrWhatsAppFailQR         = errors.New("failed to get WhatsApp QR link channel")
	ErrWhatsAppFailLink       = errors.New("failed to link with WhatsApp")
	ErrWhatsAppFailLinkDevice = errors.New("linking to WhatsApp is successful, but device ID is missing")
	ErrWhatsAppLoggedout      = errors.New("logged out from WhatsApp, please link again")
	ErrWhatsAppInvalidJID     = errors.New("cannot parse recipient JID")
	ErrWhatsAppSendingMessage = errors.New("error sending WhatsApp message")
	ErrWhatsAppDisconnected   = errors.New("WhatsApp client disconnected, will auto-reconnect")
	ErrWhatsAppOutdated       = errors.New("WhatsApp Go library version is outdated, please update")
	ErrWhatsAppBan            = errors.New("WhatsApp device is temporarily banned")

	WhatsAppQueueName        = []byte(WhatsAppQueue)
	whatsAppCli              *whatsmeow.Client
	whatsAppCliMu            sync.Mutex // guards whatsAppCli and whatsAppStore initialisation
	whatsAppStore            *sqlstore.Container
	WhatsAppVersion          = version.ReadVersion("go.mau.fi/whatsmeow")
	whatsAppGroupsMu         sync.Mutex // protects every whatsAppGroups* below and whatsAppResolvedUserIDs
	whatsAppGroupsResolved   bool       // true once groups have been successfully resolved
	whatsAppGroupsComplete   bool       // the cached resolution matched every configured name
	whatsAppGroupsResolvedAt time.Time  // when the cached resolution was made; re-resolve after whatsAppGroupsCacheTTL
	whatsAppResolvedUserIDs  []string   // resolved JIDs cached across poll cycles
	whatsAppGroupsWarned     string     // key of the last unmatched-names warning; "" once every name matches
	whatsAppGroupsPersisted  []string   // matched names already handed to the config rewrite this process

	// WhatsAppPairingMu hands the sqlstore from startup pairing to the runtime
	// client. Pairing holds it through Disconnect/Close so whatsAppInit
	// cannot race.
	WhatsAppPairingMu sync.Mutex

	// Fires SIGTERM once, however many fatal events arrive.
	shutdownOnce sync.Once
)

// RequestShutdown self-signals SIGTERM so main runs its normal graceful
// shutdown and drains the queue on the way out; logger.Fatal would bypass both.
// For unrecoverable conditions not worth crashing on: LoggedOut, PairError with
// a nil device, and a broken dedup database.
func RequestShutdown() {
	shutdownOnce.Do(func() {
		if p, err := os.FindProcess(os.Getpid()); err == nil {
			_ = p.Signal(syscall.SIGTERM)
		}
	})
}

// whatsAppSender is the slice of *whatsmeow.Client the send path uses, so tests
// can drive delivery outcomes without a paired device.
type whatsAppSender interface {
	SendMessage(ctx context.Context, to types.JID, message *waE2E.Message,
		extra ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error)
}

// whatsAppGroupLister is the slice used to resolve group names to JIDs.
type whatsAppGroupLister interface {
	GetJoinedGroups(ctx context.Context) ([]*types.GroupInfo, error)
}

// WhatsAppConfig holds the per-messenger settings for the WhatsApp backend.
type WhatsAppConfig struct {
	UserIDs []string
	Groups  []string
	Retries uint
}

// WhatsApp resolves configured group names to JIDs, resends queued failures,
// then delivers ch to the configured users and groups. On init failure it drains
// ch to the queue rather than lose flagged events.
func WhatsApp(ctx context.Context, eDB *sqlitedb.Edb, ch <-chan msgtypes.Message, cfg WhatsAppConfig) (err error) {
	// Panic guard; stays nil on the resend path (see recoverMessenger).
	var inflight *msgtypes.Message

	defer func() {
		if r := recover(); r != nil {
			err = recoverMessenger(ctx, eDB, WhatsAppQueueName, ch, r, inflight)
		}
	}()

	// Cloned: group resolution appends, which would otherwise mutate
	// cfg.UserIDs' backing array.
	userIDs := slices.Clone(cfg.UserIDs)
	groups := cfg.Groups

	if len(userIDs) == 0 && len(groups) == 0 {
		queueUndelivered(ctx, eDB, WhatsAppQueueName, ch)

		return ErrWhatsAppEmptyUserIDs
	}

	// Kept open: reconnecting costs a full state resync.
	err = whatsAppInit(ctx)
	if err != nil {
		// Connect() is network I/O, so this is reachable on transient failures.
		// Already dedup-flagged: queue them or lose them forever.
		queueUndelivered(ctx, eDB, WhatsAppQueueName, ch)

		return err
	}

	// Snapshot: only whatsAppLogin replaces the global.
	whatsAppCliMu.Lock()
	cli := whatsAppCli
	whatsAppCliMu.Unlock()

	logger.Debug().Msgf("Started WhatsApp messenger (%v, protocol %v)", WhatsAppVersion,
		store.GetWAVersion().String())

	rl := ratelimit.New(WhatsAppAPILimit, ratelimit.Per(WhatsAppWindow))

	userIDs = whatsAppEffectiveUserIDs(ctx, cli, userIDs, groups)

	resendQueued(ctx, eDB, WhatsAppQueueName, func(m msgtypes.Message) {
		processWhatsApp(ctx, cli, eDB, m, userIDs, rl, cfg.Retries)
	})

	// Drain fully; processWhatsApp durably queues on cancelled ctx, losing nothing.
	for g := range ch {
		inflight = &g
		processWhatsApp(ctx, cli, eDB, g, userIDs, rl, cfg.Retries)
		inflight = nil
	}

	return nil
}

// markWhatsAppPermanent stops retry-go on the "impossible request" sentinels:
// nil client, not logged in, malformed JID, broadcast-list target, unknown
// server. Session, websocket and timeout errors stay transient, whatsmeow
// reconnecting between attempts.
func markWhatsAppPermanent(err error) error {
	if err == nil {
		return nil
	}

	if errors.Is(err, whatsmeow.ErrClientIsNil) ||
		errors.Is(err, whatsmeow.ErrNotLoggedIn) ||
		errors.Is(err, whatsmeow.ErrRecipientADJID) ||
		errors.Is(err, whatsmeow.ErrBroadcastListUnsupported) ||
		errors.Is(err, whatsmeow.ErrUnknownServer) {
		// Inner sentinel survives retry.Do's marker stripping.
		return retry.Unrecoverable(permanentError{err})
	}

	return err
}

// processWhatsApp sends g as plain text to each recipient JID, re-queueing on
// partial or total failure. Invalid JIDs are dropped without spending rate
// budget.
func processWhatsApp(ctx context.Context, cli whatsAppSender, eDB *sqlitedb.Edb, g msgtypes.Message, userIDs []string, rl ratelimit.Limiter, retries uint) {
	// PlainMsg: Conversation rendering would interpret Markdown metacharacters.
	mRaw := truncateWithEllipsis(format.PlainMsg(g.Username, g.Subject, g.Code, g.Descriptions, g.Fields), WhatsAppMaxMessageChars)
	m := waE2E.Message{Conversation: &mRaw}

	run := newRecipientRun(g)

	for _, u := range userIDs {
		if run.skipped(u) {
			continue
		}

		// Before Take(), so shutdown isn't held by a pending token.
		if ctx.Err() != nil {
			run.interrupt()

			break
		}

		// Before Take(): an invalid JID must not spend rate budget.
		target, err := types.ParseJID(u)
		if err != nil {
			// Never becomes valid: log loudly and drop.
			logger.Error().Msgf("%v: permanently dropping recipient %q: %v", ErrWhatsAppInvalidJID, u, err)

			run.poison(u)

			continue
		}

		rl.Take()

		err = retry.New(
			retry.Attempts(retries),
			retry.Context(ctx),
			retry.Delay(WhatsAppMinDelay),
		).Do(
			func() error {
				_, err := cli.SendMessage(ctx, target, &m)

				return markWhatsAppPermanent(err)
			},
		)
		if err != nil {
			if isPermanentSendErr(err) {
				// Broadcast unsupported or unknown server: drop, don't requeue.
				logger.Error().Msgf("%v: permanently dropping recipient %q: %v", ErrWhatsAppSendingMessage, u, err)

				run.poison(u)

				continue
			}

			logger.Error().Msgf("%v: %v", ErrWhatsAppSendingMessage, err)

			run.failed()

			continue
		}

		run.delivered(u)
	}

	run.finish(ctx, eDB, WhatsAppQueueName, g)
}

// awaitWhatsAppPairingDone blocks until interactive pairing releases
// WhatsAppPairingMu. Separate so its deliberately empty critical section does
// not trip linters inside whatsAppInit.
func awaitWhatsAppPairingDone() {
	WhatsAppPairingMu.Lock()
	//nolint:staticcheck // SA2001: intentional handoff barrier, no work held under lock
	WhatsAppPairingMu.Unlock()
}

// whatsAppInit idempotently logs the shared client in. No manual reconnect:
// whatsmeow has its own goroutine for that, and racing it risks a double-connect
// conflict that forces a re-link.
func whatsAppInit(ctx context.Context) error {
	// Wait for pairing to release the sqlstore.
	awaitWhatsAppPairingDone()

	whatsAppCliMu.Lock()
	defer whatsAppCliMu.Unlock()

	if whatsAppCli == nil || whatsAppStore == nil {
		logger.Debug().Msg("Initializing WhatsApp client")

		return whatsAppLogin(ctx)
	}

	return nil
}

// filterGroupsByName returns the JIDs of joined groups named in groups, which
// config loading has sorted so the lookup can binary-search, and the distinct
// configured names that matched.
func filterGroupsByName(groups []string, joined []*types.GroupInfo) ([]string, []string) {
	jids := make([]string, 0, len(groups))

	var names []string

	for _, x := range joined {
		if _, found := slices.BinarySearch(groups, x.Name); found {
			jids = append(jids, x.JID.String())

			if !slices.Contains(names, x.Name) {
				names = append(names, x.Name)
			}
		}
	}

	return jids, names
}

// whatsAppEffectiveUserIDs merges resolved group JIDs into the static user IDs.
// The TTL, rather than a process-lifetime cache, is what lets a lost membership
// stop consuming sends: a zero-match lookup keeps the last good resolution only
// until whatsAppGroupsStaleLimit after it was made. Cache state is guarded by
// whatsAppGroupsMu.
func whatsAppEffectiveUserIDs(ctx context.Context, cli whatsAppGroupLister, userIDs, groups []string) []string {
	if len(groups) == 0 {
		return userIDs
	}

	if !whatsAppGroupsNeedsResolution() {
		whatsAppGroupsMu.Lock()
		defer whatsAppGroupsMu.Unlock()

		return whatsAppResolvedUserIDs
	}

	userIDSize := len(userIDs)

	resolved, matched, err := whatsAppProcessGroups(ctx, cli, userIDs, groups)

	switch {
	case err != nil:
		// An untouched timestamp is what makes the next cycle retry; serve
		// the last good cache meanwhile. No stale limit here, unlike a zero
		// match: a failed lookup says nothing about membership, and dropping
		// the groups would send to none of them and queue nothing.
		whatsAppGroupsMu.Lock()
		if whatsAppGroupsResolved {
			userIDs = whatsAppResolvedUserIDs
		}
		whatsAppGroupsMu.Unlock()
	case len(matched) > 0:
		userIDs = resolved

		// Incomplete keeps resolution running every cycle, so a group joined
		// later is picked up within the hour rather than after the TTL.
		unmatched := slices.DeleteFunc(slices.Clone(groups), func(n string) bool { return slices.Contains(matched, n) })

		whatsAppGroupsMu.Lock()
		whatsAppGroupsResolved = true
		whatsAppGroupsComplete = len(unmatched) == 0
		whatsAppGroupsResolvedAt = time.Now()
		whatsAppResolvedUserIDs = userIDs

		warn := len(unmatched) > 0 && whatsAppGroupsLatchWarning("partial", unmatched)
		if len(unmatched) == 0 {
			whatsAppGroupsWarned = "" // a full match re-arms every warning
		}

		// Once per name: a partial match re-resolves every cycle, and each
		// pass would otherwise re-read the config, or re-warn about a
		// read-only one, every hour.
		persist := slices.ContainsFunc(matched, func(n string) bool { return !slices.Contains(whatsAppGroupsPersisted, n) })
		if persist {
			whatsAppGroupsPersisted = append(whatsAppGroupsPersisted, matched...)
		}
		whatsAppGroupsMu.Unlock()

		if warn {
			logger.Warn().Msgf("WhatsApp groups not found: %v; delivering to the other configured groups. Verify the bot is a member and the names match exactly",
				unmatched)
		}

		// Persisted so future runs skip resolution of the matched names.
		if confFile, ok := ctx.Value(ConfFileKey).(string); persist && ok {
			whatsAppPersistResolvedGroups(confFile, resolved[userIDSize:], matched)
		}
	default:
		// Nothing matched. A list that comes back empty right after a
		// reconnect looks the same as a typo, so a recent resolution is
		// kept, like a lookup error: dropping it would send this cycle's
		// alerts to no group and queue nothing. The untouched timestamp
		// retries next cycle. Past the stale limit the bot has most likely
		// left the groups, and sending on would only fail every cycle.
		whatsAppGroupsMu.Lock()

		dropped := false

		if whatsAppGroupsResolved && time.Since(whatsAppGroupsResolvedAt) < whatsAppGroupsStaleLimit {
			userIDs = whatsAppResolvedUserIDs
		} else {
			dropped = whatsAppGroupsResolved
			whatsAppGroupsResolved = false
			whatsAppResolvedUserIDs = nil
		}

		warn := whatsAppGroupsLatchWarning("none", groups)
		whatsAppGroupsMu.Unlock()

		if warn {
			logger.Warn().Msgf("No WhatsApp groups matched the configured names %v; verify the bot is a member and the names match exactly",
				groups)
		}

		// Not latched: it happens once per cached resolution, and it is the
		// moment group delivery stops.
		if dropped {
			logger.Warn().Msgf("WhatsApp groups %v have not matched for over %v; no longer sending to them",
				groups, whatsAppGroupsStaleLimit)
		}
	}

	return userIDs
}

// whatsAppGroupsLatchWarning reports whether a warning about names, of the
// given kind, is new: a repeat of the last one is suppressed, so a typo warns
// once rather than every tick, while a different condition (a partial match
// turning into no match, or another name going missing) warns again. Callers
// hold whatsAppGroupsMu.
func whatsAppGroupsLatchWarning(kind string, names []string) bool {
	key := kind + "\x00" + strings.Join(names, "\x00")
	if key == whatsAppGroupsWarned {
		return false
	}

	whatsAppGroupsWarned = key

	return true
}

// whatsAppGroupsNeedsResolution reports whether the cache is absent or stale.
func whatsAppGroupsNeedsResolution() bool {
	whatsAppGroupsMu.Lock()
	defer whatsAppGroupsMu.Unlock()

	return !whatsAppGroupsResolved || !whatsAppGroupsComplete || time.Since(whatsAppGroupsResolvedAt) > whatsAppGroupsCacheTTL
}

// whatsAppProcessGroups appends matching joined-group JIDs to userIDs and
// reports which configured names matched. A lookup error leaves userIDs
// untouched and is returned, so the caller can fall back to the cached
// resolution.
func whatsAppProcessGroups(ctx context.Context, cli whatsAppGroupLister, userIDs, groups []string) ([]string, []string, error) {
	if len(groups) == 0 {
		return userIDs, nil, nil
	}

	g, err := cli.GetJoinedGroups(ctx)
	if err != nil {
		logger.Error().Msgf("%v %v", ErrWhatsAppUnableGroups, err)

		return userIDs, nil, err
	}

	jids, names := filterGroupsByName(groups, g)

	// Clipped: appending must not write into the caller's backing array.
	userIDs = slices.Clip(userIDs)

	for _, jid := range jids {
		// Surfaced so an operator can pin it manually.
		logger.Debug().Msgf("Found WhatsApp group and mapped to ID %v", jid)

		// Already pinned in userids as well: one send, not two.
		if !slices.Contains(userIDs, jid) {
			userIDs = append(userIDs, jid)
		}
	}

	return userIDs, names, nil
}

// whatsAppPersistResolvedGroups rewrites group names as JIDs so future runs skip
// resolution. Best effort — a failure only costs another resolution.
func whatsAppPersistResolvedGroups(confFile string, jids, names []string) {
	written := rewriteConfig(confFile, "WhatsApp group resolution", func(cfg *config.TomlConfig) bool {
		// Merged into the file as it is now, not the startup snapshot, so
		// edits made since startup survive. Only matched names leave groups:
		// an unmatched one (not joined yet, mid-rename) must stay to be
		// resolved later, not vanish from the config.
		changed := false

		for _, jid := range jids {
			if !slices.Contains(cfg.WhatsApp.UserIDs, jid) {
				cfg.WhatsApp.UserIDs = append(cfg.WhatsApp.UserIDs, jid)
				changed = true
			}
		}

		kept := slices.DeleteFunc(slices.Clone(cfg.WhatsApp.Groups), func(n string) bool { return slices.Contains(names, n) })
		if len(kept) != len(cfg.WhatsApp.Groups) {
			cfg.WhatsApp.Groups = kept
			changed = true
		}

		// The 24h re-resolution lands here again; an already-persisted
		// result must not rewrite the file each time.
		return changed
	})
	if written {
		logger.Info().Msgf("WhatsApp: moved resolved groups %v into userids in %q", names, confFile)
	}
}

// whatsAppLogin opens the store, loads the first device and connects. Globals
// are published only on full success, so a failed attempt leaves a clean slate.
func whatsAppLogin(ctx context.Context) error {
	// Partial 3-month sync, to shrink the first-link cost.
	store.DeviceProps.RequireFullSync = new(false)

	store.DeviceProps.Os = new(WhatsAppOS)
	store.DeviceProps.PlatformType = waCompanionReg.DeviceProps_DESKTOP.Enum()

	storeContainer, err := sqlstore.New(ctx, "sqlite",
		fmt.Sprintf(WhatsAppDBConnstring, WhatsAppDBName), nil)
	if err != nil {
		logger.Error().Msgf("%v: %v", ErrWhatsAppUnableConnect, err)

		return err
	}

	err = storeContainer.Upgrade(ctx)
	if err != nil {
		// Not yet published: close it or the retry leaks the sqlite handle.
		_ = storeContainer.Close()

		logger.Error().Msgf("%v: %v", ErrWhatsAppUnableUpgrade, err)

		return err
	}

	// Single-session only.
	device, err := storeContainer.GetFirstDevice(ctx)
	if err != nil {
		_ = storeContainer.Close()

		logger.Error().Msgf("%v: %v", ErrWhatsAppUnableDeviceID, err)

		return err
	}

	whatsAppStore = storeContainer
	whatsAppCli = whatsmeow.NewClient(device, nil)
	whatsAppCli.EnableAutoReconnect = true
	whatsAppCli.AutoTrustIdentity = true
	whatsAppCli.AddEventHandler(whatsAppEventHandler)

	err = whatsAppCli.Connect()
	if err != nil {
		// Release and reset, so the retry starts clean.
		_ = storeContainer.Close()
		whatsAppStore = nil
		whatsAppCli = nil

		logger.Error().Msgf("%v: %v", ErrWhatsAppFailConnect, err)

		return err
	}

	return nil
}

// whatsAppEventHandler is the runtime whatsmeow callback. LoggedOut and a
// device-less PairError delete the session and request graceful shutdown;
// stream replacement invalidates the group cache.
func whatsAppEventHandler(rawEvt any) {
	// Snapshot: whatsmeow fires callbacks outside init.
	whatsAppCliMu.Lock()
	cli := whatsAppCli
	whatsAppCliMu.Unlock()

	if cli == nil {
		return
	}

	switch evt := rawEvt.(type) {
	case *events.OfflineSyncPreview:
		logger.Debug().Msgf("WhatsApp offline sync preview: %v messages, %v receipts, %v notifications, %v app data changes",
			evt.Messages, evt.Receipts, evt.Notifications, evt.AppDataChanges)
	case *events.HistorySync:
		logger.Debug().Msg("WhatsApp history sync")
	case *events.OfflineSyncCompleted:
		logger.Debug().Msg("WhatsApp offline sync completed")
	case *events.AppStateSyncComplete:
		if len(cli.Store.PushName) > 0 && evt.Name == appstate.WAPatchCriticalBlock {
			SendPresenceBounded(cli, types.PresenceAvailable)
			SendPresenceBounded(cli, types.PresenceUnavailable)
		}

		logger.Debug().Msg("WhatsApp app state sync completed")
	case *events.Connected, *events.PushNameSetting:
		if len(cli.Store.PushName) > 0 {
			SendPresenceBounded(cli, types.PresenceAvailable)
			SendPresenceBounded(cli, types.PresenceUnavailable)
		}
	case *events.PairError:
		// Fatal only when unpaired: a healthy client can see a spurious
		// PairError from a stale attempt and must not self-logout.
		if cli.Store.ID == nil {
			RemoveWhatsAppSession()

			logger.Error().Msgf("%v — requesting shutdown", ErrWhatsAppFailLinkDevice)
			RequestShutdown()
		} else {
			logger.Warn().Msgf("Ignoring WhatsApp pairing error on an already-paired client: %v", evt.Error)
		}
	case *events.PairSuccess:
		if cli.Store.ID == nil {
			RemoveWhatsAppSession()

			logger.Error().Msgf("%v — requesting shutdown", ErrWhatsAppFailLinkDevice)
			RequestShutdown()
		} else {
			logger.Debug().Msg("WhatsApp device successfully paired")
		}
	case *events.LoggedOut:
		RemoveWhatsAppSession()

		logger.Error().Msgf("%v — requesting shutdown", ErrWhatsAppLoggedout)
		RequestShutdown()
	case *events.Disconnected:
		logger.Debug().Msgf("%v", ErrWhatsAppDisconnected)
	case *events.StreamReplaced, *events.KeepAliveTimeout:
		logger.Debug().Msgf("%v", ErrWhatsAppDisconnected)

		// Server-side replacement may have staled the JIDs.
		whatsAppGroupsMu.Lock()
		whatsAppGroupsResolved = false
		whatsAppResolvedUserIDs = nil
		whatsAppGroupsMu.Unlock()
	case *events.ClientOutdated:
		logger.Error().Msgf("%v", ErrWhatsAppOutdated)
	case *events.TemporaryBan:
		duration := durafmt.Parse(evt.Expire).String()
		logger.Error().Msgf("%v: code %v / expire %v", ErrWhatsAppBan, evt.Code, duration)
	default:
	}
}

// RemoveWhatsAppSession deletes the paired-device store so the next run links
// afresh. Failing silently would have callers telling the operator to re-link
// while the dead session persists, looping on the same fatal event.
func RemoveWhatsAppSession() {
	if err := removeWhatsAppSession(WhatsAppDBName); err != nil {
		logger.Error().Msgf("Unable to remove the WhatsApp session store %q: %v — delete it manually before restarting, or the next run will reuse the dead session",
			WhatsAppDBName, err)
	}
}

// removeWhatsAppSession is the testable core. An absent store counts as
// success: repeated fatal events race each other through here. The WAL
// sidecars go too, as SQLite would replay a stale -wal into a fresh store of
// the same name; the store itself goes last so a failure leaves it visible.
func removeWhatsAppSession(path string) error {
	var errs []error

	for _, p := range []string{path + "-wal", path + "-shm", path} {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

// isWriteable reports whether path opens for writing.
func isWriteable(path string) bool {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return false
	}

	_ = f.Close()

	return true
}
