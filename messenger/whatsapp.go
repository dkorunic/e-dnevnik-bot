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

	"github.com/avast/retry-go/v4"
	"github.com/dkorunic/e-dnevnik-bot/format"
	"github.com/dkorunic/e-dnevnik-bot/logger"
	"github.com/dkorunic/e-dnevnik-bot/msgtypes"
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

const (
	DefaultWhatsAppDBName       = ".e-dnevnik.sqlite"
	DefaultWhatsAppDBConnstring = "file:%v?_pragma=foreign_keys(1)"
	DefaultWhatsAppDisplayName  = "Chrome (Linux)"
	DefaultWhatsAppOS           = "Linux"
)

const (
	WhatsAppAPILimit = 10 // 10 req/s per user/IP
	WhatsAppWindow   = 1 * time.Second
	WhatsAppMinDelay = WhatsAppWindow / WhatsAppAPILimit
)

var (
	ErrWhatsAppEmptyUserIDs   = errors.New("empty list of WhatsApp UserIDs (JIDs)")
	ErrWhatsAppUnableConnect  = errors.New("unable to connect to WhatsApp database")
	ErrWhatsAppUnableUpgrade  = errors.New("unable to upgrade WhatsApp database")
	ErrWhatsAppUnableDeviceID = errors.New("unable to get WhatsApp device ID")
	ErrWhatsaAppUnableGroups  = errors.New("unable to list WhatsApp groups")
	ErrWhatsAppFailConnect    = errors.New("failed to connect to WhatsApp")
	ErrWhatsAppFailQR         = errors.New("failed to get WhatsApp QR link channel")
	ErrWhatsAppFailLink       = errors.New("failed to link with WhatsApp")
	ErrWhatsAppFailLinkDevice = errors.New("linking to WhatsApp is successful, but device ID is missing")
	ErrWhatsAppLoggedout      = errors.New("logged out from WhatsApp, please link again")
	ErrWhatsAppInvalidJID     = errors.New("cannot parse recipient JID")
	ErrWhatsAppSendingMessage = errors.New("error sending WhatsApp message")
	ErrWhatsAppDisconnected   = errors.New("WhatsApp client disconnected, will auto-reconnect")

	whatsAppCli *whatsmeow.Client
)

// WhatsApp sends messages through the WhatsApp API to the specified user IDs.
//
// ctx: The context.Context used for handling cancellations and deadlines.
// ch: The channel from which to receive messages.
// userIDs: A slice of strings containing the IDs of the recipients (JIDs).
// retries: The number of attempts to retry sending a message in case of failure.
//
// The function returns an error if there was a problem sending the message.
// It ensures the client is connected or reconnects if needed, and uses rate limiting
// to control the message sending rate. Messages are formatted and sent as Markup.
// If the userIDs slice is empty, it returns an ErrWhatsAppEmptyUserIDs error.
func WhatsApp(ctx context.Context, ch <-chan interface{}, userIDs, groups []string, retries uint) error {
	if len(userIDs) == 0 && len(groups) == 0 {
		return ErrWhatsAppEmptyUserIDs
	}

	// reconnecting and syncing the state is very expensive, so we are keeping it open
	// and reconnecting only if needed
	err := whatsAppInit()
	if err != nil {
		return err
	}

	logger.Debug().Msg("Started WhatsApp messenger")

	rl := ratelimit.New(WhatsAppAPILimit, ratelimit.Per(WhatsAppWindow))

	// find named groups and append to userIDs
	userIDs = whatsAppProcessGroups(userIDs, groups)

	for o := range ch {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			g, ok := o.(msgtypes.Message)
			if !ok {
				logger.Warn().Msg("Received invalid type from channel, trying to continue")

				continue
			}

			// format message as Markup
			mRaw := format.MarkupMsg(g.Username, g.Subject, g.IsExam, g.Descriptions, g.Fields)
			m := &waE2E.Message{Conversation: proto.String(mRaw)}

			// send to all recipients: channels and nicknames are permitted
			for _, u := range userIDs {
				rl.Take()

				target, err := types.ParseJID(u)
				if err != nil {
					logger.Error().Msgf("%v: %v", ErrWhatsAppInvalidJID, err)

					continue
				}

				// retryable and cancellable attempt to send a message
				err = retry.Do(
					func() error {
						_, err := whatsAppCli.SendMessage(ctx, target, m)

						return err
					},
					retry.Attempts(retries),
					retry.Context(ctx),
					retry.Delay(WhatsAppMinDelay),
				)
				if err != nil {
					logger.Error().Msgf("%v: %v", ErrWhatsAppSendingMessage, err)

					break
				}
			}
		}
	}

	return err
}

// whatsAppInit ensures that the WhatsApp client is initialized and connected.
//
// If the WhatsApp client is not already initialized, it calls whatsAppLogin()
// to initialize it. If the client is initialized but not connected, it attempts
// to disconnect and then reconnect the client. The function returns an error
// if any step of the initialization or connection process fails.
func whatsAppInit() error {
	if whatsAppCli == nil {
		return whatsAppLogin()
	} else if !whatsAppCli.IsConnected() {
		whatsAppCli.Disconnect()

		return whatsAppCli.Connect()
	}

	return nil
}

// whatsAppProcessGroups takes a list of group names and user IDs, retrieves the joined WhatsApp groups,
// and appends the JIDs of any matching groups to the user IDs slice. If an error occurs while fetching
// the groups, it logs the error. The function returns the updated list of user IDs which now includes
// the JIDs of the specified groups.
func whatsAppProcessGroups(userIDs, groups []string) []string {
	if len(groups) > 0 {
		g, err := whatsAppCli.GetJoinedGroups()
		if err != nil {
			logger.Error().Msgf("%v %v", ErrWhatsaAppUnableGroups, err)

			return userIDs
		}

		for _, x := range g {
			// have a matching group name, add group JID to userIDs
			if _, found := slices.BinarySearch(groups, x.Name); found {
				userIDs = append(userIDs, x.JID.String())

				// debugging for users to help directly write JID to config userIDs
				logger.Debug().Msgf("Found WhatsApp group by name %q - ID %q", x.Name, x.JID.String())
			}
		}
	}

	return userIDs
}

// whatsAppLogin initializes WhatsApp messenger.
//
// It connects to the SQLite database specified by DefaultWhatsAppDBName, upgrades
// the database schema if necessary, loads the first device, and then connects
// to WhatsApp using the loaded device. If the device is not logged in, it will
// request a QR code or code to pair with the WhatsApp account.
//
// The WhatsApp client is configured to enable automatic reconnection and
// automatic trust of the identity key. The client is also configured to use
// the whatsAppEventHandler to handle events.
//
// If any error occurs during the process, it is logged and returned.
func whatsAppLogin() error {
	// request syncing for last 3-months
	store.DeviceProps.RequireFullSync = proto.Bool(false)

	// set OS to Linux
	store.DeviceProps.Os = proto.String(DefaultWhatsAppOS)

	storeContainer, err := sqlstore.New("sqlite",
		fmt.Sprintf(DefaultWhatsAppDBConnstring, DefaultWhatsAppDBName), nil)
	if err != nil {
		logger.Error().Msgf("%v: %v", ErrWhatsAppUnableConnect, err)

		return err
	}

	err = storeContainer.Upgrade()
	if err != nil {
		logger.Error().Msgf("%v: %v", ErrWhatsAppUnableUpgrade, err)

		return err
	}

	// use only first device, we don't support multiple sessions
	device, err := storeContainer.GetFirstDevice()
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
func whatsAppEventHandler(rawEvt interface{}) {
	switch evt := rawEvt.(type) {
	case *events.AppStateSyncComplete:
		if len(whatsAppCli.Store.PushName) > 0 && evt.Name == appstate.WAPatchCriticalBlock {
			_ = whatsAppCli.SendPresence(types.PresenceAvailable)
		}

		logger.Debug().Msg("WhatsApp online sync completed")
	case *events.Connected, *events.PushNameSetting:
		if len(whatsAppCli.Store.PushName) > 0 {
			_ = whatsAppCli.SendPresence(types.PresenceAvailable)
		}
	case *events.PairSuccess, *events.PairError:
		if whatsAppCli.Store.ID == nil {
			_ = os.Remove(DefaultWhatsAppDBName)

			logger.Fatal().Msgf("%v", ErrWhatsAppFailLinkDevice)
		}
	case *events.LoggedOut:
		_ = os.Remove(DefaultWhatsAppDBName)

		logger.Fatal().Msgf("%v", ErrWhatsAppLoggedout)
	case *events.Disconnected, *events.StreamReplaced, *events.KeepAliveTimeout:
		logger.Debug().Msgf("%v", ErrWhatsAppDisconnected)
	}
}
