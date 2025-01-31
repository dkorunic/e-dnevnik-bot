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

package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"time"

	"github.com/dkorunic/e-dnevnik-bot/config"
	"github.com/dkorunic/e-dnevnik-bot/logger"
	"github.com/dkorunic/e-dnevnik-bot/messenger"
	"github.com/mattn/go-isatty"
	"github.com/mdp/qrterminal/v3"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
	_ "modernc.org/sqlite"
)

var (
	whatsAppCli    *whatsmeow.Client
	whatsAppSynced = make(chan struct{})
)

// init initializes the GitTag, GitCommit, GitDirty, and BuildTime variables.
//
// It trims leading and trailing white spaces from the values of GitTag, GitCommit,
// GitDirty, and BuildTime.
//
//nolint:gochecknoinits
func init() {
	GitTag = strings.TrimSpace(GitTag)
	GitCommit = strings.TrimSpace(GitCommit)
	GitDirty = strings.TrimSpace(GitDirty)
	BuildTime = strings.TrimSpace(BuildTime)
}

// checkCalendarConf checks the Calendar configuration and enables or disables the Calendar integration based on the existence of the Google Calendar API credentials file and token file.
//
// Parameters:
// - config: a pointer to the TomlConfig struct containing the configuration settings.
// - ctx: the context object for cancellation and timeout.
func checkCalendar(ctx context.Context, config *config.TomlConfig) {
	if config == nil {
		return
	}

	if _, err := os.Stat(*calTokFile); errors.Is(err, fs.ErrNotExist) {
		// checkWhatsAppConf if we are running under a terminal
		if isTerminal() {
			logger.Error().Msg("Google Calendar API token file not found and first run requires running under a terminal. Disabling Calendar integration.")

			config.CalendarEnabled = false
		} else {
			// early Google Calendar API initialization and token refresh
			_, _, err := messenger.InitCalendar(ctx, *calTokFile, config.Calendar.Name)
			if err != nil {
				logger.Error().Msgf("Error initializing Google Calendar API: %v. Disabling Calendar integration.", err)

				config.CalendarEnabled = false
			}
		}
	}
}

// checkWhatsApp checks the WhatsApp configuration and enables or disables the WhatsApp integration based on the existence of the WhatsApp device ID.
//
// It will request syncing for the last 3-months and print the QR code if the WhatsApp device ID is not found in the database. It will also handle the pairing process if the WhatsApp device ID is found.
//
// Parameters:
// - ctx: the context object for cancellation and timeout.
// - config: a pointer to the TomlConfig struct containing the configuration settings.
func checkWhatsApp(ctx context.Context, config *config.TomlConfig) {
	if config == nil {
		return
	}

	// request syncing for last 3-months
	store.DeviceProps.RequireFullSync = proto.Bool(false)

	// set OS to Linux
	store.DeviceProps.Os = proto.String(messenger.WhatsAppOS)

	storeContainer, err := sqlstore.New("sqlite",
		fmt.Sprintf(messenger.WhatsAppDBConnstring, messenger.WhatsAppDBName), nil)
	if err != nil {
		logger.Fatal().Msgf("%v: %v", messenger.ErrWhatsAppUnableConnect, err)
	}
	defer storeContainer.Close()

	err = storeContainer.Upgrade()
	if err != nil {
		logger.Fatal().Msgf("%v: %v", messenger.ErrWhatsAppUnableUpgrade, err)
	}

	// use only first device, we don't support multiple sessions
	device, err := storeContainer.GetFirstDevice()
	if err != nil {
		logger.Fatal().Msgf("%v: %v", messenger.ErrWhatsAppUnableDeviceID, err)
	}

	whatsAppCli = whatsmeow.NewClient(device, nil)
	whatsAppCli.EnableAutoReconnect = true
	whatsAppCli.AutoTrustIdentity = true
	whatsAppCli.AddEventHandler(whatsappPairingEventHandler)

	if whatsAppCli.Store.ID != nil {
		logger.Debug().Msg("Already paired with WhatsApp, skipping pairing process")
	}

	// prepare pairing through QR or pair code
	ch, err := whatsAppCli.GetQRChannel(ctx)
	if err != nil {
		if !errors.Is(err, whatsmeow.ErrQRStoreContainsID) {
			logger.Fatal().Msgf("%v: %v", messenger.ErrWhatsAppFailQR, err)
		} else {
			logger.Info().Msg("Logged in to WhatsApp")

			return
		}
	}

	// handle QR code and initiate code pairing
	go func() {
		for evt := range ch {
			// pair/link event
			if evt.Event == "code" {
				// prefer pairing through code if phone number is enabled
				if config.WhatsApp.PhoneNumber != "" {
					linkCode, err := whatsAppCli.PairPhone(config.WhatsApp.PhoneNumber, true,
						whatsmeow.PairClientChrome, messenger.WhatsAppDisplayName)
					if err != nil {
						logger.Fatal().Msgf("%v: %v", messenger.ErrWhatsAppFailLink, err)
					}

					logger.Info().Msgf("WhatsApp link code: %v", linkCode)
				} else {
					// display QR code only on interactive terminal
					if isTerminal() {
						logger.Info().Msg("WhatsApp QR code below")
						qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, os.Stdout)
					} else {
						logger.Fatal().Msg("WhatsApp first run requires running under a terminal or pairing through phone number.")
					}
				}
			}
		}
	}()

	err = whatsAppCli.Connect()
	if err != nil {
		logger.Fatal().Msgf("Failed to connect to WhatsApp: %v", err)
	}
	defer whatsAppCli.Disconnect()

	logger.Info().Msg("Please wait until WhatsApp has fully synced and keep mobile app open. This can take quite a while.")
	<-whatsAppSynced

	// give additional time for WhatsApp to fully sync
	time.Sleep(120 * time.Second)
}

// whatsappPairingEventHandler is a callback function that handles events from the
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
func whatsappPairingEventHandler(rawEvt interface{}) {
	switch evt := rawEvt.(type) {
	case *events.OfflineSyncCompleted:
		logger.Debug().Msg("WhatsApp offline sync completed")
	case *events.AppStateSyncComplete:
		if len(whatsAppCli.Store.PushName) > 0 && evt.Name == appstate.WAPatchCriticalBlock {
			_ = whatsAppCli.SendPresence(types.PresenceAvailable)
		}

		logger.Debug().Msg("WhatsApp app state sync completed")

		whatsAppSynced <- struct{}{}
	case *events.Connected, *events.PushNameSetting:
		if len(whatsAppCli.Store.PushName) > 0 {
			_ = whatsAppCli.SendPresence(types.PresenceAvailable)
		}
	case *events.PairSuccess, *events.PairError:
		if whatsAppCli.Store.ID == nil {
			_ = os.Remove(messenger.WhatsAppDBName)

			logger.Fatal().Msgf("%v", messenger.ErrWhatsAppFailLinkDevice)
		}
	case *events.LoggedOut:
		_ = os.Remove(messenger.WhatsAppDBName)

		logger.Fatal().Msgf("%v", messenger.ErrWhatsAppLoggedout)
	case *events.Disconnected, *events.StreamReplaced, *events.KeepAliveTimeout:
		logger.Debug().Msgf("%v", messenger.ErrWhatsAppDisconnected)
	}
}

// isTerminal checks if the current output is a terminal.
//
// It returns true if the environment does not disable color output, the terminal
// is not set to "dumb", and the output file descriptor is a terminal. It also
// considers Cygwin terminals as valid terminals.
func isTerminal() bool {
	fd := os.Stdout.Fd()

	return os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb" &&
		(isatty.IsTerminal(fd) || isatty.IsCygwinTerminal(fd))
}
