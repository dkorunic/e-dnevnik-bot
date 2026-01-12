// @license
// Copyright (C) 2025  Dinko Korunic
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

package messenger

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"time"

	"github.com/avast/retry-go/v5"
	"github.com/dkorunic/e-dnevnik-bot/config"
	"github.com/dkorunic/e-dnevnik-bot/db"
	"github.com/dkorunic/e-dnevnik-bot/format"
	"github.com/dkorunic/e-dnevnik-bot/logger"
	"github.com/dkorunic/e-dnevnik-bot/msgtypes"
	"github.com/dkorunic/e-dnevnik-bot/queue"
	"github.com/dkorunic/e-dnevnik-bot/version"
	"github.com/hako/durafmt"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"go.uber.org/ratelimit"
	"google.golang.org/protobuf/proto"
	_ "modernc.org/sqlite" // register pure-Go sqlite database/sql driver
)

type contextKey string

const (
	WhatsAppDBName                  = ".e-dnevnik.sqlite"
	WhatsAppDBConnstring            = "file:%v?_pragma=foreign_keys(1)&_pragma=busy_timeout=10000"
	WhatsAppDisplayName             = "Chrome (Linux)"
	WhatsAppOS                      = "Linux"
	WhatsAppAPILimit                = 10 // 10 req/min per user/IP
	WhatsAppWindow                  = 1 * time.Minute
	WhatsAppMinDelay                = WhatsAppWindow / WhatsAppAPILimit
	WhatsAppQueue                   = "whatsapp-queue"
	confFileKey          contextKey = "confFile"
)

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

	WhatsAppQueueName = []byte(WhatsAppQueue)
	whatsAppCli       *whatsmeow.Client
	WhatsAppVersion   = version.ReadVersion("go.mau.fi/whatsmeow")
)

// WhatsApp sends messages through the WhatsApp API to the specified user IDs or groups.
//
// It takes the following parameters:
// - ctx: the context.Context object for managing the execution of the function.
// - eDB: the database instance for checking and storing failed messages.
// - ch: the channel from which to receive messages.
// - userIDs: a slice of strings containing the WhatsApp user IDs (JIDs) to send the message to.
// - groups: a slice of strings containing the names of the WhatsApp groups to send the message to.
// - retries: the number of retry attempts for sending the message.
//
// The function processes all failed messages, finds named groups and appends them to the userIDs,
// processes all new messages and sends them to the specified user IDs or groups.
// It logs errors for invalid chat IDs, sending failures, and stores failed messages for retry.
// It uses rate limiting and supports retries with delay.
func WhatsApp(ctx context.Context, eDB *db.Edb, ch <-chan msgtypes.Message, userIDs, groups []string, retries uint) error {
	if len(userIDs) == 0 && len(groups) == 0 {
		return ErrWhatsAppEmptyUserIDs
	}

	// reconnecting and syncing the state is very expensive, so we are keeping it open
	// and reconnecting only if needed
	err := whatsAppInit(ctx)
	if err != nil {
		return err
	}

	logger.Debug().Msgf("Started WhatsApp messenger (%v, protocol %v)", WhatsAppVersion,
		store.GetWAVersion().String())

	rl := ratelimit.New(WhatsAppAPILimit, ratelimit.Per(WhatsAppWindow))

	userIDSize := len(userIDs)

	// find named groups and append to userIDs
	userIDs = whatsAppProcessGroups(ctx, userIDs, groups)

	// rewrite config if groups are specified and userIDs have changed
	if len(groups) > 0 && len(userIDs) > userIDSize {
		if confFile, ok := ctx.Value(confFileKey).(string); ok {
			if isWriteable(confFile) {
				logger.Info().Msg("Detected WhatsApp group with a name instead of userID, rewriting configuration")

				cfg, err := config.LoadConfig(confFile)
				if err != nil {
					logger.Error().Msgf("Error loading configuration: %v", err)
				} else {
					cfg.WhatsApp.UserIDs = userIDs
					cfg.WhatsApp.Groups = []string{}

					err = config.SaveConfig(confFile, cfg)
					if err != nil {
						logger.Error().Msgf("Error saving configuration: %v", err)
					}
				}
			} else {
				logger.Info().Msg("Detected WhatsApp group with a name but rewriting configuration is impossible due to lack of permissions")
			}
		}
	}

	var g msgtypes.Message

	// process failed messages
	for _, g = range queue.FetchFailedMsgs(eDB, WhatsAppQueueName) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			processWhatsApp(ctx, eDB, g, userIDs, rl, retries)
		}
	}

	// process all new messages
	for g = range ch {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			processWhatsApp(ctx, eDB, g, userIDs, rl, retries)
		}
	}

	return nil
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
func processWhatsApp(ctx context.Context, eDB *db.Edb, g msgtypes.Message, userIDs []string, rl ratelimit.Limiter, retries uint) {
	// format message as Markup
	mRaw := format.MarkupMsg(g.Username, g.Subject, g.Code, g.Descriptions, g.Fields)
	m := waE2E.Message{Conversation: proto.String(mRaw)}

	// send to all recipients: channels and nicknames are permitted
	for _, u := range userIDs {
		rl.Take()

		target, err := types.ParseJID(u)
		if err != nil {
			logger.Error().Msgf("%v: %v", ErrWhatsAppInvalidJID, err)

			continue
		}

		// retryable and cancellable attempt to send a message
		err = retry.New(
			retry.Attempts(retries),
			retry.Context(ctx),
			retry.Delay(WhatsAppMinDelay),
		).Do(
			func() error {
				_, err := whatsAppCli.SendMessage(ctx, target, &m)

				return err
			},
		)
		if err != nil {
			logger.Error().Msgf("%v: %v", ErrWhatsAppSendingMessage, err)

			// store failed message
			if err := queue.StoreFailedMsgs(eDB, WhatsAppQueueName, g); err != nil {
				logger.Error().Msgf("%v: %v", queue.ErrQueueing, err)
			}

			continue
		}
	}
}

// whatsAppInit ensures that the WhatsApp client is initialized and connected.
//
// If the WhatsApp client is not already initialized, it calls whatsAppLogin()
// to initialize it. If the client is initialized but not connected, it attempts
// to disconnect and then reconnect the client. The function returns an error
// if any step of the initialization or connection process fails.
func whatsAppInit(ctx context.Context) error {
	if whatsAppCli == nil {
		logger.Debug().Msg("Initializing WhatsApp client")

		return whatsAppLogin(ctx)
	} else if !whatsAppCli.IsConnected() {
		logger.Debug().Msg("Reconnecting WhatsApp client")

		whatsAppCli.Disconnect()

		return whatsAppCli.Connect()
	}

	return nil
}

// whatsAppProcessGroups takes a list of group names and user IDs, retrieves the joined WhatsApp groups,
// and appends the JIDs of any matching groups to the user IDs slice. If an error occurs while fetching
// the groups, it logs the error. The function returns the updated list of user IDs which now includes
// the JIDs of the specified groups.
func whatsAppProcessGroups(ctx context.Context, userIDs, groups []string) []string {
	if len(groups) > 0 {
		g, err := whatsAppCli.GetJoinedGroups(ctx)
		if err != nil {
			logger.Error().Msgf("%v %v", ErrWhatsAppUnableGroups, err)

			return userIDs
		}

		for _, x := range g {
			// have a matching group name, add group JID to userIDs
			if _, found := slices.BinarySearch(groups, x.Name); found {
				userIDs = append(userIDs, x.JID.String())

				// debugging for users to help directly write JID to config userIDs
				logger.Debug().Msgf("Found WhatsApp group by name %v and mapped to ID %v", x.Name, x.JID.String())
			}
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
	// request syncing for last 3-months
	store.DeviceProps.RequireFullSync = proto.Bool(false)

	// set OS to Linux
	store.DeviceProps.Os = proto.String(WhatsAppOS)

	storeContainer, err := sqlstore.New(ctx, "sqlite",
		fmt.Sprintf(WhatsAppDBConnstring, WhatsAppDBName), nil)
	if err != nil {
		logger.Error().Msgf("%v: %v", ErrWhatsAppUnableConnect, err)

		return err
	}

	err = storeContainer.Upgrade(ctx)
	if err != nil {
		logger.Error().Msgf("%v: %v", ErrWhatsAppUnableUpgrade, err)

		return err
	}

	// use only first device, we don't support multiple sessions
	device, err := storeContainer.GetFirstDevice(ctx)
	if err != nil {
		logger.Error().Msgf("%v: %v", ErrWhatsAppUnableDeviceID, err)

		return err
	}

	whatsAppCli = whatsmeow.NewClient(device, nil)
	whatsAppCli.EnableAutoReconnect = true
	whatsAppCli.AutoTrustIdentity = true
	whatsAppCli.AddEventHandler(whatsAppEventHandler)

	err = whatsAppCli.Connect()
	if err != nil {
		logger.Error().Msgf("%v: %v", ErrWhatsAppFailConnect, err)
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
	switch evt := rawEvt.(type) {
	case *events.OfflineSyncPreview:
		logger.Debug().Msgf("WhatsApp offline sync preview: %v messages, %v receipts, %v notifications, %v app data changes",
			evt.Messages, evt.Receipts, evt.Notifications, evt.AppDataChanges)
	case *events.HistorySync:
		logger.Debug().Msg("WhatsApp history sync")
	case *events.OfflineSyncCompleted:
		logger.Debug().Msg("WhatsApp offline sync completed")
	case *events.AppStateSyncComplete:
		if len(whatsAppCli.Store.PushName) > 0 && evt.Name == appstate.WAPatchCriticalBlock {
			_ = whatsAppCli.SendPresence(context.Background(), types.PresenceAvailable)
			_ = whatsAppCli.SendPresence(context.Background(), types.PresenceUnavailable)
		}

		logger.Debug().Msg("WhatsApp app state sync completed")
	case *events.Connected, *events.PushNameSetting:
		if len(whatsAppCli.Store.PushName) > 0 {
			_ = whatsAppCli.SendPresence(context.Background(), types.PresenceAvailable)
			_ = whatsAppCli.SendPresence(context.Background(), types.PresenceUnavailable)
		}
	case *events.PairSuccess, *events.PairError:
		if whatsAppCli.Store.ID == nil {
			_ = os.Remove(WhatsAppDBName)

			logger.Fatal().Msgf("%v", ErrWhatsAppFailLinkDevice)
		} else {
			logger.Debug().Msg("WhatsApp device successfully paired")
		}
	case *events.LoggedOut:
		_ = os.Remove(WhatsAppDBName)

		logger.Fatal().Msgf("%v", ErrWhatsAppLoggedout)
	case *events.Disconnected, *events.StreamReplaced, *events.KeepAliveTimeout:
		logger.Debug().Msgf("%v", ErrWhatsAppDisconnected)
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
