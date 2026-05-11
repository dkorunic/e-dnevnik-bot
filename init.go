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
	"github.com/hako/durafmt"
	"github.com/mattn/go-isatty"
	"github.com/mdp/qrterminal/v3"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	_ "modernc.org/sqlite"
)

const (
	initialWhatsAppDelay = 2 * time.Minute // 2 minutes sleep after successful sync
	WhatsAppDBOldName    = ".e-dnevnik.sqlite"

	// whatsAppPresenceTimeout bounds SendPresence calls so a stalled WhatsApp
	// socket cannot block pairing/event-handling callbacks indefinitely. 5 s
	// is well above round-trip latency for healthy sessions yet short enough
	// to surface a dead link quickly.
	whatsAppPresenceTimeout = 5 * time.Second

	// whatsAppPairingTimeout bounds the interactive pairing/sync wait so that
	// a user who walks away from the QR code or PIN prompt does not leave the
	// process hanging indefinitely. 10 minutes is well above realistic scan
	// latency yet short enough to surface a stalled pairing on the next CI run
	// or systemd retry.
	whatsAppPairingTimeout = 10 * time.Minute
)

var (
	whatsAppPairingCli *whatsmeow.Client
	whatsAppSynced     = make(chan struct{}, 1)
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
		if !isTerminal() {
			logger.Warn().Msgf("Google Calendar token file %q not found; first-run OAuth requires an interactive terminal. Disabling Calendar integration.", *calTokFile)

			config.CalendarEnabled = false
		} else {
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
// It will request syncing for the last 3-months and print the QR code if the WhatsApp device ID is not found in the database. It will also handle the pairing process if the WhatsApp device ID is not found.
//
// Parameters:
// - ctx: the context object for cancellation and timeout.
// - config: a pointer to the TomlConfig struct containing the configuration settings.
func checkWhatsApp(ctx context.Context, config *config.TomlConfig) {
	if config == nil {
		return
	}

	// LIFO defers ensure Disconnect/Close complete before messenger's whatsAppInit sees the lock released.
	messenger.WhatsAppPairingMu.Lock()
	defer messenger.WhatsAppPairingMu.Unlock()

	if fi, err := os.Stat(WhatsAppDBOldName); err == nil && fi.Mode().IsRegular() {
		logger.Debug().Msgf("Found old WhatsApp DB %v, renaming to %v", WhatsAppDBOldName, messenger.WhatsAppDBName)
		_ = os.Rename(WhatsAppDBOldName, messenger.WhatsAppDBName)
	}

	// Request 3-month sync only.
	store.DeviceProps.RequireFullSync = new(false)

	store.DeviceProps.Os = new(messenger.WhatsAppOS)

	storeContainer, err := sqlstore.New(ctx, "sqlite",
		fmt.Sprintf(messenger.WhatsAppDBConnstring, messenger.WhatsAppDBName), nil)
	if err != nil {
		logger.Fatal().Msgf("%v: %v", messenger.ErrWhatsAppUnableConnect, err)
	}

	defer storeContainer.Close()

	err = storeContainer.Upgrade(ctx)
	if err != nil {
		logger.Fatal().Msgf("%v: %v", messenger.ErrWhatsAppUnableUpgrade, err)
	}

	// Single-device only; multi-session is unsupported.
	device, err := storeContainer.GetFirstDevice(ctx)
	if err != nil {
		logger.Fatal().Msgf("%v: %v", messenger.ErrWhatsAppUnableDeviceID, err)
	}

	whatsAppPairingCli = whatsmeow.NewClient(device, nil)
	whatsAppPairingCli.EnableAutoReconnect = true
	whatsAppPairingCli.AutoTrustIdentity = true
	whatsAppPairingCli.AddEventHandler(whatsappPairingEventHandler)

	qrCtx, qrCancel := context.WithCancel(ctx)
	defer qrCancel()

	ch, err := whatsAppPairingCli.GetQRChannel(qrCtx)
	if err != nil {
		if !errors.Is(err, whatsmeow.ErrQRStoreContainsID) {
			logger.Fatal().Msgf("%v: %v", messenger.ErrWhatsAppFailQR, err)
		} else {
			logger.Info().Msg("Logged in to WhatsApp")

			return
		}
	}

	//nolint:nestif
	go func() {
		for evt := range ch {
			if evt.Event == "code" {
				if config.WhatsApp.PhoneNumber != "" {
					linkCode, err := whatsAppPairingCli.PairPhone(qrCtx, config.WhatsApp.PhoneNumber, true,
						whatsmeow.PairClientChrome, messenger.WhatsAppDisplayName)
					if err != nil {
						logger.Fatal().Msgf("%v: %v", messenger.ErrWhatsAppFailLink, err)
					}

					logger.Info().Msgf("WhatsApp link code: %v", linkCode)
				} else {
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

	err = whatsAppPairingCli.Connect()
	if err != nil {
		logger.Fatal().Msgf("Failed to connect to WhatsApp: %v", err)
	}

	defer whatsAppPairingCli.Disconnect()

	logger.Info().Msg("Please wait until WhatsApp has fully synced and keep Android/iOS mobile app active and open")

	select {
	case <-whatsAppSynced:
	case <-ctx.Done():
		logger.Warn().Msg("Context cancelled while waiting for WhatsApp sync")

		return
	case <-time.After(whatsAppPairingTimeout):
		logger.Warn().Msgf("Timed out after %v waiting for WhatsApp pairing/sync; restart the bot and re-scan the QR code or re-enter the PIN",
			durafmt.Parse(whatsAppPairingTimeout).String())

		return
	}

	logger.Info().Msg("Waiting for 2 more minutes for WhatsApp mobile app to acknowledge completed transfer")

	// Extra grace period for full sync; honours cancellation.
	select {
	case <-time.After(initialWhatsAppDelay):
	case <-ctx.Done():
		logger.Warn().Msg("Context cancelled while waiting for WhatsApp post-sync delay")
	}
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
func whatsappPairingEventHandler(rawEvt any) {
	switch evt := rawEvt.(type) {
	case *events.OfflineSyncPreview:
		logger.Info().Msgf("WhatsApp offline sync preview: %v messages, %v receipts, %v notifications, %v app data changes",
			evt.Messages, evt.Receipts, evt.Notifications, evt.AppDataChanges)
	case *events.HistorySync:
		logger.Info().Msg("WhatsApp history sync")
	case *events.OfflineSyncCompleted:
		logger.Info().Msg("WhatsApp offline sync completed")
	case *events.AppStateSyncComplete:
		if len(whatsAppPairingCli.Store.PushName) > 0 && evt.Name == appstate.WAPatchCriticalBlock {
			presCtx, presCancel := context.WithTimeout(context.Background(), whatsAppPresenceTimeout)
			_ = whatsAppPairingCli.SendPresence(presCtx, types.PresenceAvailable)
			_ = whatsAppPairingCli.SendPresence(presCtx, types.PresenceUnavailable)
			presCancel()
		}

		logger.Info().Msg("WhatsApp app state sync completed")

		select {
		case whatsAppSynced <- struct{}{}:
		default:
		}
	case *events.Connected, *events.PushNameSetting:
		if len(whatsAppPairingCli.Store.PushName) > 0 {
			presCtx, presCancel := context.WithTimeout(context.Background(), whatsAppPresenceTimeout)
			_ = whatsAppPairingCli.SendPresence(presCtx, types.PresenceAvailable)
			_ = whatsAppPairingCli.SendPresence(presCtx, types.PresenceUnavailable)
			presCancel()
		}
	case *events.PairSuccess, *events.PairError:
		if whatsAppPairingCli.Store.ID == nil {
			_ = os.Remove(messenger.WhatsAppDBName)

			logger.Fatal().Msgf("%v", messenger.ErrWhatsAppFailLinkDevice)
		} else {
			logger.Info().Msg("WhatsApp device successfully paired")
		}
	case *events.LoggedOut:
		_ = os.Remove(messenger.WhatsAppDBName)

		logger.Fatal().Msgf("%v", messenger.ErrWhatsAppLoggedout)
	case *events.Disconnected, *events.StreamReplaced, *events.KeepAliveTimeout:
		logger.Debug().Msgf("%v", messenger.ErrWhatsAppDisconnected)
	case *events.ClientOutdated:
		logger.Error().Msgf("%v", messenger.ErrWhatsAppOutdated)
	case *events.TemporaryBan:
		duration := durafmt.Parse(evt.Expire).String()
		logger.Fatal().Msgf("%v: code %v / expire %v", messenger.ErrWhatsAppBan, evt.Code, duration)
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
