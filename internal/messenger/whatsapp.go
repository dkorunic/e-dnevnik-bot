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
	WhatsAppDBConnstring            = "file:%v?_pragma=foreign_keys(1)&_pragma=busy_timeout=10000"
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
	whatsAppGroupsMu         sync.Mutex // protects whatsAppGroupsResolved*, whatsAppResolvedUserIDs and whatsAppGroupsWarned
	whatsAppGroupsResolved   bool       // true once groups have been successfully resolved
	whatsAppGroupsResolvedAt time.Time  // when the cached resolution was made; re-resolve after whatsAppGroupsCacheTTL
	whatsAppResolvedUserIDs  []string   // resolved JIDs cached across poll cycles
	whatsAppGroupsWarned     bool       // "no group matched" warning latch; re-armed on each successful resolution

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
// config loading has sorted so the lookup can binary-search.
func filterGroupsByName(groups []string, joined []*types.GroupInfo) []string {
	jids := make([]string, 0, len(groups))

	for _, x := range joined {
		if _, found := slices.BinarySearch(groups, x.Name); found {
			jids = append(jids, x.JID.String())
		}
	}

	return jids
}

// whatsAppEffectiveUserIDs merges resolved group JIDs into the static user IDs.
// The TTL, rather than a process-lifetime cache, is what lets a lost membership
// stop consuming sends. Cache state is guarded by whatsAppGroupsMu.
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

	resolved, err := whatsAppProcessGroups(ctx, cli, userIDs, groups)

	switch {
	case err != nil:
		// An untouched timestamp is what makes the next cycle retry; serve
		// the last good cache meanwhile.
		whatsAppGroupsMu.Lock()
		if whatsAppGroupsResolved {
			userIDs = whatsAppResolvedUserIDs
		}
		whatsAppGroupsMu.Unlock()
	case len(resolved) > userIDSize:
		userIDs = resolved

		whatsAppGroupsMu.Lock()
		whatsAppGroupsResolved = true
		whatsAppGroupsResolvedAt = time.Now()
		whatsAppResolvedUserIDs = userIDs
		whatsAppGroupsWarned = false // re-arm the no-match warning
		whatsAppGroupsMu.Unlock()

		// Persisted so future runs skip resolution.
		if confFile, ok := ctx.Value(ConfFileKey).(string); ok {
			whatsAppPersistResolvedGroups(confFile, userIDs)
		}
	default:
		// Removed from the groups, or a typo: drop the stale cache so dead
		// groups stop receiving sends.
		whatsAppGroupsMu.Lock()
		whatsAppGroupsResolved = false
		whatsAppResolvedUserIDs = nil

		// Latched: a typo warns once, not every tick.
		warn := !whatsAppGroupsWarned
		whatsAppGroupsWarned = true
		whatsAppGroupsMu.Unlock()

		if warn {
			logger.Warn().Msgf("No WhatsApp groups matched the configured names %v; verify the bot is a member and the names match exactly",
				groups)
		}
	}

	return userIDs
}

// whatsAppGroupsNeedsResolution reports whether the cache is absent or stale.
func whatsAppGroupsNeedsResolution() bool {
	whatsAppGroupsMu.Lock()
	defer whatsAppGroupsMu.Unlock()

	return !whatsAppGroupsResolved || time.Since(whatsAppGroupsResolvedAt) > whatsAppGroupsCacheTTL
}

// whatsAppProcessGroups appends matching joined-group JIDs to userIDs. A lookup
// error leaves userIDs untouched and is returned, so the caller can fall back to
// the cached resolution.
func whatsAppProcessGroups(ctx context.Context, cli whatsAppGroupLister, userIDs, groups []string) ([]string, error) {
	if len(groups) > 0 {
		g, err := cli.GetJoinedGroups(ctx)
		if err != nil {
			logger.Error().Msgf("%v %v", ErrWhatsAppUnableGroups, err)

			return userIDs, err
		}

		for _, jid := range filterGroupsByName(groups, g) {
			userIDs = append(userIDs, jid)

			// Surfaced so an operator can pin it manually.
			logger.Debug().Msgf("Found WhatsApp group and mapped to ID %v", jid)
		}
	}

	return userIDs, nil
}

// whatsAppPersistResolvedGroups rewrites group names as JIDs so future runs skip
// resolution. Best effort — a failure only costs another resolution.
func whatsAppPersistResolvedGroups(confFile string, userIDs []string) {
	configRewriteMu.Lock()
	defer configRewriteMu.Unlock()

	if !isWriteable(confFile) {
		logger.Info().Msg("Detected WhatsApp group with a name but rewriting configuration is impossible due to lack of permissions")

		return
	}

	logger.Info().Msg("Detected WhatsApp group with a name instead of userID, rewriting configuration")

	// Non-validating: LoadConfig's fail-fast validators would os.Exit this
	// goroutine on a broken mid-run edit.
	cfg, err := config.LoadConfigRaw(confFile)
	if err != nil {
		logger.Error().Msgf("Error loading configuration: %v", err)

		return
	}

	cfg.WhatsApp.UserIDs = userIDs
	cfg.WhatsApp.Groups = []string{}

	if err = config.SaveConfig(confFile, cfg); err != nil {
		logger.Error().Msgf("Error saving configuration: %v", err)
	}
}

// whatsAppLogin opens the store, loads the first device and connects. Globals
// are published only on full success, so a failed attempt leaves a clean slate.
func whatsAppLogin(ctx context.Context) error {
	// Partial 3-month sync, to shrink the first-link cost.
	store.DeviceProps.RequireFullSync = new(false)

	store.DeviceProps.Os = new(WhatsAppOS)

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
// success: repeated fatal events race each other through here.
func removeWhatsAppSession(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	return nil
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
