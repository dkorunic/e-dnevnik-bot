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

// SendPresenceBounded sends a presence update with its own bounded context.
// Errors are intentionally discarded; presence is best-effort signalling and
// the caller cannot recover from a failed send. Each call gets a fresh
// whatsAppPresenceTimeout budget so a slow first call does not zero out the
// budget for a subsequent one.
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

	// shutdownOnce ensures requestShutdown fires SIGTERM exactly once across repeated fatal events.
	shutdownOnce sync.Once
)

// requestShutdown signals SIGTERM to the current process so that the main
// loop's signal.NotifyContext catches it and runs the normal graceful
// shutdown path — including draining the failed-message queue. This replaces
// the prior logger.Fatal()/os.Exit() pattern in event handlers, which
// bypassed queue persistence on unrecoverable WhatsApp events (LoggedOut,
// PairError with nil device).
func requestShutdown() {
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

// WhatsApp sends messages through the WhatsApp API to the specified user IDs or groups.
//
// It takes the following parameters:
// - ctx: the context.Context object for managing the execution of the function.
// - eDB: the database instance for checking and storing failed messages.
// - ch: the channel from which to receive messages.
// - cfg: the WhatsApp messenger configuration (user IDs, group names, retries).
//
// The function processes all failed messages, finds named groups and appends them to the userIDs,
// processes all new messages and sends them to the specified user IDs or groups.
// It logs errors for invalid chat IDs, sending failures, and stores failed messages for retry.
// It uses rate limiting and supports retries with delay.
func WhatsApp(ctx context.Context, eDB *sqlitedb.Edb, ch <-chan msgtypes.Message, cfg WhatsAppConfig) error {
	// Local aliases: group resolution below mutates userIDs in place.
	userIDs := cfg.UserIDs
	groups := cfg.Groups

	if len(userIDs) == 0 && len(groups) == 0 {
		return ErrWhatsAppEmptyUserIDs
	}

	// Keep connection open; reconnect is expensive (full state resync).
	err := whatsAppInit(ctx)
	if err != nil {
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

	// Drain failed-message queue; requeue leftovers if ctx cancels mid-drain.
	failedMsgs := queue.FetchFailedMsgs(ctx, eDB, WhatsAppQueueName)
	for i, g := range failedMsgs {
		if ctx.Err() != nil {
			queue.RequeueMsgs(ctx, eDB, WhatsAppQueueName, failedMsgs[i:])

			return ctx.Err()
		}

		processWhatsApp(ctx, cli, eDB, g, userIDs, rl, cfg.Retries)

		if ctx.Err() != nil {
			queue.RequeueMsgs(ctx, eDB, WhatsAppQueueName, failedMsgs[i+1:])

			return ctx.Err()
		}
	}

	// Drain fully; processWhatsApp durably queues on cancelled ctx, losing nothing.
	for g := range ch {
		processWhatsApp(ctx, cli, eDB, g, userIDs, rl, cfg.Retries)
	}

	return nil
}

// markWhatsAppPermanent wraps whatsmeow errors that will never succeed on
// retry in retry.Unrecoverable so retry-go short-circuits the remaining
// attempts.
//
// Permanent categories are sentinels whose meaning is "the request as
// formulated is impossible": nil client, not logged in, malformed recipient
// JID, broadcast-list target, unknown server. Transient errors — session not
// yet established, websocket not connected, IQ/message timeouts — fall
// through unchanged because whatsmeow auto-reconnects and rebuilds session
// state between attempts.
func markWhatsAppPermanent(err error) error {
	if err == nil {
		return nil
	}

	if errors.Is(err, whatsmeow.ErrClientIsNil) ||
		errors.Is(err, whatsmeow.ErrNotLoggedIn) ||
		errors.Is(err, whatsmeow.ErrRecipientADJID) ||
		errors.Is(err, whatsmeow.ErrBroadcastListUnsupported) ||
		errors.Is(err, whatsmeow.ErrUnknownServer) {
		return retry.Unrecoverable(err)
	}

	return err
}

// processWhatsApp processes a message and sends it to the specified user IDs on WhatsApp.
//
// It takes the following parameters:
// - ctx: the context.Context object for managing the execution of the function.
// - eDB: the database instance for checking failed messages.
// - g: the message to be processed.
// - userIDs: a slice of strings containing the IDs of the recipients (JIDs).
// - rl: the rate limiter to control the message sending rate.
// - retries: the number of retry attempts to send the message.
//
// It formats the message as Markup and attempts to send it to each user ID.
// It logs errors for invalid chat IDs, sending failures, and stores failed messages for retry.
// It uses rate limiting and supports retries with delay.
func processWhatsApp(ctx context.Context, cli *whatsmeow.Client, eDB *sqlitedb.Edb, g msgtypes.Message, userIDs []string, rl ratelimit.Limiter, retries uint) {
	// PlainMsg avoids leaking Markdown metacharacters via Conversation rendering.
	mRaw := truncateWithEllipsis(format.PlainMsg(g.Username, g.Subject, g.Code, g.Descriptions, g.Fields), WhatsAppMaxMessageChars)
	m := waE2E.Message{Conversation: &mRaw}

	skipSet := make(map[string]struct{}, len(g.SkipRecipients))
	for _, r := range g.SkipRecipients {
		skipSet[r] = struct{}{}
	}

	var successfulIDs []string

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
			logger.Error().Msgf("%v: %v", ErrWhatsAppInvalidJID, err)

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
			logger.Error().Msgf("%v: %v", ErrWhatsAppSendingMessage, err)

			anyFailed = true

			continue
		}

		successfulIDs = append(successfulIDs, u)
	}

	if anyFailed || !allProcessed {
		// Dedup prevents unbounded SkipRecipients growth across retries.
		g.SkipRecipients = mergeSkipRecipients(g.SkipRecipients, successfulIDs)

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

// whatsAppInit ensures that the WhatsApp client is initialized.
//
// If the client is not already initialized, it calls whatsAppLogin() to do so.
// Once initialized, transient disconnects are handled by whatsmeow's own
// reconnect goroutine (EnableAutoReconnect = true). A manual Disconnect/Connect
// path here would race the library's reconnect and can cause double-connect
// session conflicts that force a re-link.
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

// whatsAppProcessGroups takes a list of group names and user IDs, retrieves the joined WhatsApp groups,
// and appends the JIDs of any matching groups to the user IDs slice. If an error occurs while fetching
// the groups, it logs the error. The function returns the updated list of user IDs which now includes
// the JIDs of the specified groups.
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

// whatsAppLogin initializes WhatsApp messenger.
//
// It connects to the SQLite database specified by WhatsAppDBName, upgrades
// the database schema if necessary, loads the first device, and then connects
// to WhatsApp using the loaded device. If the device is not logged in, it will
// request a QR code or code to pair with the WhatsApp account.
//
// The WhatsApp client is configured to enable automatic reconnection and
// automatic trust of the identity key. The client is also configured to use
// the whatsAppEventHandler to handle events.
//
// If any error occurs during the process, it is logged and returned.
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

// whatsAppEventHandler is a callback function that handles events from the
// WhatsApp client. It switches on the type of the event and performs the
// appropriate action.
//
// Events handled:
//   - *events.AppStateSyncComplete: checks if the client has a push name and
//     sends an available presence if so. Logs a message if the online sync
//     completed and signals that the WhatsApp client is fully synced.
//   - *events.Connected, *events.PushNameSetting: checks if the client has a
//     push name and sends an available presence if so.
//   - *events.PairSuccess, *events.PairError: checks if the client has a device
//     ID and removes the database file if not. Logs a message if the linking
//     process failed.
//   - *events.LoggedOut: removes the database file and logs a message.
//   - *events.Disconnected, *events.StreamReplaced, *events.KeepAliveTimeout:
//     logs a message, disconnects the client, and reconnects if possible.
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
	case *events.PairSuccess, *events.PairError:
		if cli.Store.ID == nil {
			_ = os.Remove(WhatsAppDBName)

			// Trigger graceful shutdown via SIGTERM-to-self so the main
			// loop drains the failed-message queue before exiting.
			logger.Error().Msgf("%v — requesting shutdown", ErrWhatsAppFailLinkDevice)
			requestShutdown()
		} else {
			logger.Debug().Msg("WhatsApp device successfully paired")
		}
	case *events.LoggedOut:
		_ = os.Remove(WhatsAppDBName)

		logger.Error().Msgf("%v — requesting shutdown", ErrWhatsAppLoggedout)
		requestShutdown()
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

// isWriteable checks if a file at the specified path is writable.
//
// It takes the following parameter:
// - path: the path to the file to check.
//
// It returns true if the file is writable, false otherwise.
//
// It opens the file in write-only mode and checks if the open operation
// succeeds. If it succeeds, it closes the file and returns true. If it
// fails, it returns false.
func isWriteable(path string) bool {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return false
	}

	_ = f.Close()

	return true
}
