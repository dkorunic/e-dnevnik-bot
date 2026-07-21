// SPDX-FileCopyrightText: 2025 Dinko Korunic
// SPDX-License-Identifier: MIT

package messenger

import (
	"context"
	"errors"
	"fmt"
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
	"github.com/dkorunic/e-dnevnik-bot/internal/queue"
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

// ContextKey is a string alias used for context keys to avoid collisions.
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

	// whatsAppPresenceTimeout bounds each SendPresence call so a stalled socket can't block the callback goroutine.
	whatsAppPresenceTimeout = 5 * time.Second
)

// SendPresenceBounded sends a best-effort presence update under its own
// whatsAppPresenceTimeout so a stalled socket can't block the caller. Errors
// are unrecoverable and discarded.
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

	WhatsAppQueueName       = []byte(WhatsAppQueue)
	whatsAppCli             *whatsmeow.Client
	whatsAppCliMu           sync.Mutex // guards whatsAppCli and whatsAppStore initialisation
	whatsAppStore           *sqlstore.Container
	WhatsAppVersion         = version.ReadVersion("go.mau.fi/whatsmeow")
	whatsAppGroupsMu        sync.Mutex // protects whatsAppGroupsResolved and whatsAppResolvedUserIDs
	whatsAppGroupsResolved  bool       // true once groups have been successfully resolved
	whatsAppResolvedUserIDs []string   // resolved JIDs cached across poll cycles
	whatsAppGroupsWarnOnce  sync.Once  // ensures the "no group matched" warning logs at most once per process

	// WhatsAppPairingMu hands off the sqlstore from startup pairing to the runtime client.
	// Pairing code holds it through Disconnect/Close so whatsAppInit cannot race.
	WhatsAppPairingMu sync.Mutex

	// shutdownOnce ensures RequestShutdown fires SIGTERM exactly once across repeated fatal events.
	shutdownOnce sync.Once
)

// RequestShutdown signals SIGTERM to the current process so that the main
// loop's signal.NotifyContext catches it and runs the normal graceful
// shutdown path — including draining the failed-message queue. This replaces
// the logger.Fatal()/os.Exit() pattern, which bypasses queue persistence and
// deferred cleanup. Used for unrecoverable WhatsApp events (LoggedOut,
// PairError with nil device) and exported for other unrecoverable-but-not-
// worth-crashing conditions such as a broken dedup database in main.
func RequestShutdown() {
	shutdownOnce.Do(func() {
		if p, err := os.FindProcess(os.Getpid()); err == nil {
			_ = p.Signal(syscall.SIGTERM)
		}
	})
}

// WhatsAppConfig holds the per-messenger settings for the WhatsApp backend.
type WhatsAppConfig struct {
	UserIDs []string
	Groups  []string
	Retries uint
}

// WhatsApp resolves any configured group names to JIDs, resends queued
// failures, then delivers live messages from ch to the configured user IDs and
// groups. On init failure it drains ch into the queue so already-dedup-flagged
// events are not lost.
func WhatsApp(ctx context.Context, eDB *sqlitedb.Edb, ch <-chan msgtypes.Message, cfg WhatsAppConfig) (err error) {
	// A send-path panic must drain ch and degrade, not crash the process.
	// inflight: nil on the resend path (queue row persists); set only while draining ch.
	var inflight *msgtypes.Message

	defer func() {
		if r := recover(); r != nil {
			err = recoverMessenger(ctx, eDB, WhatsAppQueueName, ch, r, inflight)
		}
	}()

	// Clone: group resolution appends to userIDs, which would otherwise mutate
	// cfg.UserIDs' backing array.
	userIDs := slices.Clone(cfg.UserIDs)
	groups := cfg.Groups

	if len(userIDs) == 0 && len(groups) == 0 {
		queueUndelivered(ctx, eDB, WhatsAppQueueName, ch)

		return ErrWhatsAppEmptyUserIDs
	}

	// Keep connection open; reconnect is expensive (full state resync).
	err = whatsAppInit(ctx)
	if err != nil {
		// Connect() is a network operation, so this path is reachable on
		// transient failures. Events are already dedup-flagged; queue them or
		// they are lost forever.
		queueUndelivered(ctx, eDB, WhatsAppQueueName, ch)

		return err
	}

	// Snapshot under mutex; only whatsAppLogin replaces the global.
	whatsAppCliMu.Lock()
	cli := whatsAppCli
	whatsAppCliMu.Unlock()

	logger.Debug().Msgf("Started WhatsApp messenger (%v, protocol %v)", WhatsAppVersion,
		store.GetWAVersion().String())

	rl := ratelimit.New(WhatsAppAPILimit, ratelimit.Per(WhatsAppWindow))

	// Retry group resolution per tick; sync.Once would strand transient failures.
	if len(groups) > 0 {
		whatsAppGroupsMu.Lock()
		needsResolution := !whatsAppGroupsResolved
		whatsAppGroupsMu.Unlock()

		if needsResolution {
			userIDSize := len(userIDs)

			userIDs = whatsAppProcessGroups(ctx, cli, userIDs, groups)

			if len(userIDs) > userIDSize {
				// Cache on first success to skip future resolution.
				whatsAppGroupsMu.Lock()
				whatsAppGroupsResolved = true
				whatsAppResolvedUserIDs = userIDs
				whatsAppGroupsMu.Unlock()

				// Persist resolved JIDs so future runs skip resolution.
				//nolint:nestif
				if confFile, ok := ctx.Value(ConfFileKey).(string); ok {
					if isWriteable(confFile) {
						logger.Info().Msg("Detected WhatsApp group with a name instead of userID, rewriting configuration")

						cfg, err := config.LoadConfig(confFile)
						if err != nil {
							logger.Error().Msgf("Error loading configuration: %v", err)
						} else {
							cfg.WhatsApp.UserIDs = userIDs
							cfg.WhatsApp.Groups = []string{}

							if err = config.SaveConfig(confFile, cfg); err != nil {
								logger.Error().Msgf("Error saving configuration: %v", err)
							}
						}
					} else {
						logger.Info().Msg("Detected WhatsApp group with a name but rewriting configuration is impossible due to lack of permissions")
					}
				}
			} else {
				// Warn once so a typo in group names surfaces without spamming every tick.
				whatsAppGroupsWarnOnce.Do(func() {
					logger.Warn().Msgf("No WhatsApp groups matched the configured names %v; verify the bot is a member and the names match exactly",
						groups)
				})
			}
			// Cache empty on failure — next tick retries.
		} else {
			whatsAppGroupsMu.Lock()
			userIDs = whatsAppResolvedUserIDs
			whatsAppGroupsMu.Unlock()
		}
	}

	// Resend queued failures first. Rows are only removed after processing, so
	// a crash mid-loop re-delivers instead of losing; on shutdown, unprocessed
	// rows simply stay queued for the next run.
	for _, q := range queue.FetchFailedMsgs(ctx, eDB, WhatsAppQueueName) {
		if ctx.Err() != nil {
			break
		}

		processWhatsApp(ctx, cli, eDB, q.Msg, userIDs, rl, cfg.Retries)

		// Failures were re-queued by processWhatsApp; drop the original row.
		queue.Dequeue(ctx, eDB, q.Key)
	}

	// Drain fully; processWhatsApp durably queues on cancelled ctx, losing nothing.
	for g := range ch {
		inflight = &g
		processWhatsApp(ctx, cli, eDB, g, userIDs, rl, cfg.Retries)
		inflight = nil
	}

	return nil
}

// markWhatsAppPermanent marks "request is impossible" sentinels (nil client,
// not logged in, malformed JID, broadcast-list target, unknown server) as
// unrecoverable so retry-go stops retrying. Session/websocket/timeout errors
// stay transient — whatsmeow auto-reconnects between attempts.
func markWhatsAppPermanent(err error) error {
	if err == nil {
		return nil
	}

	if errors.Is(err, whatsmeow.ErrClientIsNil) ||
		errors.Is(err, whatsmeow.ErrNotLoggedIn) ||
		errors.Is(err, whatsmeow.ErrRecipientADJID) ||
		errors.Is(err, whatsmeow.ErrBroadcastListUnsupported) ||
		errors.Is(err, whatsmeow.ErrUnknownServer) {
		// permanentError inside: survives retry.Do's marker stripping.
		return retry.Unrecoverable(permanentError{err})
	}

	return err
}

// processWhatsApp renders g as plain text and sends it to each recipient JID,
// re-queueing on partial or total failure. Invalid JIDs are skipped without
// spending rate budget; recipients already in SkipRecipients are omitted.
func processWhatsApp(ctx context.Context, cli *whatsmeow.Client, eDB *sqlitedb.Edb, g msgtypes.Message, userIDs []string, rl ratelimit.Limiter, retries uint) {
	// PlainMsg avoids leaking Markdown metacharacters via Conversation rendering.
	mRaw := truncateWithEllipsis(format.PlainMsg(g.Username, g.Subject, g.Code, g.Descriptions, g.Fields), WhatsAppMaxMessageChars)
	m := waE2E.Message{Conversation: &mRaw}

	skipSet := make(map[string]struct{}, len(g.SkipRecipients))
	for _, r := range g.SkipRecipients {
		skipSet[r] = struct{}{}
	}

	var successfulIDs []string

	// Permanently-failed recipients: skipped on retry, never requeued.
	var poisonedIDs []string

	anyFailed := false
	// False on mid-loop cancel forces requeue so sends aren't dropped.
	allProcessed := true

	for _, u := range userIDs {
		if _, skip := skipSet[u]; skip {
			continue
		}

		// Check before Take() so shutdown isn't held by a pending token.
		if ctx.Err() != nil {
			allProcessed = false

			break
		}

		// Validate before Take(): invalid JIDs must not spend rate budget.
		target, err := types.ParseJID(u)
		if err != nil {
			// A malformed JID never becomes valid: log loudly and drop it.
			logger.Error().Msgf("%v: permanently dropping recipient %q: %v", ErrWhatsAppInvalidJID, u, err)

			poisonedIDs = append(poisonedIDs, u)

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
				// Permanent (broadcast unsupported, unknown server): drop, don't requeue.
				logger.Error().Msgf("%v: permanently dropping recipient %q: %v", ErrWhatsAppSendingMessage, u, err)

				poisonedIDs = append(poisonedIDs, u)

				continue
			}

			logger.Error().Msgf("%v: %v", ErrWhatsAppSendingMessage, err)

			anyFailed = true

			continue
		}

		successfulIDs = append(successfulIDs, u)
	}

	if anyFailed || !allProcessed {
		// Skip successful and poisoned recipients on retry; dedup bounds growth.
		g.SkipRecipients = mergeSkipRecipients(g.SkipRecipients, append(successfulIDs, poisonedIDs...))

		sctx, scancel := queueStoreCtx(ctx)
		if err := queue.StoreFailedMsgs(sctx, eDB, WhatsAppQueueName, g); err != nil {
			logger.Error().Msgf("%v: %v", queue.ErrQueueing, err)
		}

		scancel()
	}
}

// awaitWhatsAppPairingDone blocks until any in-progress interactive pairing
// (held by the main package via WhatsAppPairingMu) has released the lock. It
// exists as a separate function so the intentional empty critical section does
// not trip linters in whatsAppInit.
func awaitWhatsAppPairingDone() {
	WhatsAppPairingMu.Lock()
	//nolint:staticcheck // SA2001: intentional handoff barrier, no work held under lock
	WhatsAppPairingMu.Unlock()
}

// whatsAppInit lazily logs in the shared WhatsApp client (idempotent). It does
// no manual reconnect: whatsmeow's own reconnect goroutine handles disconnects,
// and racing it risks double-connect conflicts that force a re-link.
func whatsAppInit(ctx context.Context) error {
	// Wait for interactive pairing to release the sqlstore.
	awaitWhatsAppPairingDone()

	whatsAppCliMu.Lock()
	defer whatsAppCliMu.Unlock()

	if whatsAppCli == nil || whatsAppStore == nil {
		logger.Debug().Msg("Initializing WhatsApp client")

		return whatsAppLogin(ctx)
	}

	return nil
}

// filterGroupsByName returns the JID strings of joined groups whose Name is
// present in the groups slice. groups must be sorted in ascending order (as
// ensured by config loading) so that binary search can be used.
func filterGroupsByName(groups []string, joined []*types.GroupInfo) []string {
	jids := make([]string, 0, len(groups))

	for _, x := range joined {
		if _, found := slices.BinarySearch(groups, x.Name); found {
			jids = append(jids, x.JID.String())
		}
	}

	return jids
}

// whatsAppProcessGroups appends the JIDs of joined groups matching the given
// names to userIDs. On lookup error it logs and returns userIDs unchanged.
func whatsAppProcessGroups(ctx context.Context, cli *whatsmeow.Client, userIDs, groups []string) []string {
	if len(groups) > 0 {
		g, err := cli.GetJoinedGroups(ctx)
		if err != nil {
			logger.Error().Msgf("%v %v", ErrWhatsAppUnableGroups, err)

			return userIDs
		}

		for _, jid := range filterGroupsByName(groups, g) {
			userIDs = append(userIDs, jid)

			// Surface JID so operators can pin it manually.
			logger.Debug().Msgf("Found WhatsApp group and mapped to ID %v", jid)
		}
	}

	return userIDs
}

// whatsAppLogin opens the WhatsApp store DB, loads the first device, and
// connects with auto-reconnect and auto-trust enabled. Globals are only
// published on full success so a failed attempt leaves a clean slate to retry.
func whatsAppLogin(ctx context.Context) error {
	// Partial 3-month sync to shrink first-link cost.
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
		// Not yet published to whatsAppStore; close to avoid leaking the sqlite handle on retry.
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
		// Release handle and reset globals so retry starts clean.
		_ = storeContainer.Close()
		whatsAppStore = nil
		whatsAppCli = nil

		logger.Error().Msgf("%v: %v", ErrWhatsAppFailConnect, err)

		return err
	}

	return nil
}

// whatsAppEventHandler is the runtime whatsmeow callback. Unrecoverable events
// (LoggedOut, PairError with no device ID) delete the session DB and request a
// graceful shutdown; stream-replacement events invalidate the group cache.
func whatsAppEventHandler(rawEvt any) {
	// Snapshot under mutex: whatsmeow fires callbacks outside init.
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
		// Fatal only when unpaired: a healthy paired client can see a spurious
		// PairError (e.g. a stale pairing attempt) and must not self-logout.
		if cli.Store.ID == nil {
			_ = os.Remove(WhatsAppDBName)

			logger.Error().Msgf("%v — requesting shutdown", ErrWhatsAppFailLinkDevice)
			RequestShutdown()
		} else {
			logger.Warn().Msgf("Ignoring WhatsApp pairing error on an already-paired client: %v", evt.Error)
		}
	case *events.PairSuccess:
		if cli.Store.ID == nil {
			_ = os.Remove(WhatsAppDBName)

			logger.Error().Msgf("%v — requesting shutdown", ErrWhatsAppFailLinkDevice)
			RequestShutdown()
		} else {
			logger.Debug().Msg("WhatsApp device successfully paired")
		}
	case *events.LoggedOut:
		_ = os.Remove(WhatsAppDBName)

		logger.Error().Msgf("%v — requesting shutdown", ErrWhatsAppLoggedout)
		RequestShutdown()
	case *events.Disconnected:
		logger.Debug().Msgf("%v", ErrWhatsAppDisconnected)
	case *events.StreamReplaced, *events.KeepAliveTimeout:
		logger.Debug().Msgf("%v", ErrWhatsAppDisconnected)

		// Force fresh group resolve: server-side replacement may stale JIDs.
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

// isWriteable reports whether path can be opened for writing.
func isWriteable(path string) bool {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return false
	}

	_ = f.Close()

	return true
}
