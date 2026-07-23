// SPDX-FileCopyrightText: 2025 Dinko Korunic
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"time"

	"github.com/dkorunic/e-dnevnik-bot/internal/config"
	"github.com/dkorunic/e-dnevnik-bot/internal/logger"
	"github.com/dkorunic/e-dnevnik-bot/internal/messenger"
	"github.com/dkorunic/e-dnevnik-bot/internal/oauth"
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

	// whatsAppPairingTimeout bounds interactive pairing so an absent user can't hang the process.
	whatsAppPairingTimeout = 10 * time.Minute
)

var (
	whatsAppPairingCli *whatsmeow.Client
	whatsAppSynced     = make(chan struct{}, 1)
)

// init trims whitespace from the ldflags-injected build vars.
//
//nolint:gochecknoinits
func init() {
	GitTag = strings.TrimSpace(GitTag)
	GitCommit = strings.TrimSpace(GitCommit)
	GitDirty = strings.TrimSpace(GitDirty)
	BuildTime = strings.TrimSpace(BuildTime)
}

// checkCalendar runs first-run Calendar OAuth when no token file exists,
// deferring the integration (a queue-only stub preserves exams) if setup fails,
// no interactive terminal is present, or the token file cannot be stat'd.
func checkCalendar(ctx context.Context, config *config.TomlConfig) {
	if config == nil {
		return
	}

	// deferCal disables live Calendar but leaves the queue-only stub (via
	// CalendarDeferred) so exams are preserved until OAuth is completed.
	deferCal := func() {
		config.CalendarEnabled = false
		config.CalendarDeferred = true
	}

	_, err := os.Stat(*calTokFile)

	switch {
	case err == nil:
		// Token present — but it must at least decode: a truncated or
		// hand-edited file would otherwise fail every poll cycle at runtime.
		if verr := oauth.ValidateTokenFile(*calTokFile); verr != nil {
			logger.Error().Msgf("Google Calendar token file %q is unreadable (%v). Deferring Calendar integration (exams will be queued); delete the file and re-run interactively to re-authenticate.",
				*calTokFile, verr)
			deferCal()
		}
	case !errors.Is(err, fs.ErrNotExist):
		// Unexpected stat error (e.g. EACCES): defer rather than fail confusingly later.
		logger.Error().Msgf("Cannot stat Google Calendar token file %q: %v. Deferring Calendar integration.", *calTokFile, err)
		deferCal()
	case !isTerminal():
		logger.Warn().Msgf("Google Calendar token file %q not found; first-run OAuth requires an interactive terminal. Deferring Calendar integration (exams will be queued for delivery once OAuth is completed).", *calTokFile)
		deferCal()
	default:
		if _, _, err := messenger.InitCalendar(ctx, *calTokFile, config.Calendar.Name); err != nil {
			logger.Error().Msgf("Error initializing Google Calendar API: %v. Deferring Calendar integration (exams will be queued for retry).", err)
			deferCal()
		}
	}
}

// checkWhatsApp runs first-run WhatsApp pairing (QR code or phone PIN) when the
// device is not yet linked, requesting a 3-month history sync. It holds
// WhatsAppPairingMu so the runtime client can't race the store handoff.
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

	// Cancel the QR context before disconnecting (LIFO: this runs first) so a
	// late "code" event can't PairPhone on a tearing-down client.
	defer func() {
		qrCancel()
		whatsAppPairingCli.Disconnect()
	}()

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

// whatsappPairingEventHandler is the pairing-time whatsmeow callback. It
// signals whatsAppSynced once app-state sync completes; a failed link or logout
// deletes the session DB and is fatal (still in interactive setup, no queue yet).
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
			messenger.SendPresenceBounded(whatsAppPairingCli, types.PresenceAvailable)
			messenger.SendPresenceBounded(whatsAppPairingCli, types.PresenceUnavailable)
		}

		logger.Info().Msg("WhatsApp app state sync completed")

		select {
		case whatsAppSynced <- struct{}{}:
		default:
		}
	case *events.Connected, *events.PushNameSetting:
		if len(whatsAppPairingCli.Store.PushName) > 0 {
			messenger.SendPresenceBounded(whatsAppPairingCli, types.PresenceAvailable)
			messenger.SendPresenceBounded(whatsAppPairingCli, types.PresenceUnavailable)
		}
	case *events.PairError:
		// Always a failure, regardless of any stale Store.ID.
		_ = os.Remove(messenger.WhatsAppDBName)

		logger.Fatal().Msgf("%v", messenger.ErrWhatsAppFailLinkDevice)
	case *events.PairSuccess:
		// A success without a stored device ID is really a failure.
		if whatsAppPairingCli.Store.ID == nil {
			_ = os.Remove(messenger.WhatsAppDBName)

			logger.Fatal().Msgf("%v", messenger.ErrWhatsAppFailLinkDevice)
		}

		logger.Info().Msg("WhatsApp device successfully paired")
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

// isTerminal reports whether stdout is an interactive terminal, gating
// first-run flows. TERM=dumb counts as non-interactive. NO_COLOR does not gate
// here — it controls colour only (see initLog); a NO_COLOR TTY is interactive.
func isTerminal() bool {
	fd := os.Stdout.Fd()

	return os.Getenv("TERM") != "dumb" &&
		(isatty.IsTerminal(fd) || isatty.IsCygwinTerminal(fd))
}
