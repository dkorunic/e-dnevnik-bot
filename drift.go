// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/dkorunic/e-dnevnik-bot/internal/logger"
	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
	"github.com/dkorunic/e-dnevnik-bot/internal/queue"
	"github.com/dkorunic/e-dnevnik-bot/internal/sqlitedb"
)

const (
	// driftSeen marks a first sighting. A teacher adding a grade between the
	// grades and course-list fetches looks like drift for exactly one cycle.
	driftSeen = "seen"

	// driftWeekPrefix marks a confirmed drift already alerted in that week.
	driftWeekPrefix = "week:"

	// driftCleanPrefix marks a clean scrape since the alert it prefixes, so a
	// flapping portal cannot alert twice in one week.
	driftCleanPrefix = "clean:"

	// driftMaxFindings bounds the alert; the log carries the full list.
	driftMaxFindings = 8

	driftSubject = "e-Dnevnik"
	driftWarning = "Portal e-Dnevnik izgleda drugačije nego što bot očekuje, pa obavijesti o ocjenama možda ne stižu ili su netočne. " +
		"Provjerite ocjene izravno na portalu i nadogradite bota."
)

// driftKeyPrefix keys each user's drift state; cannot collide with hashes.
var driftKeyPrefix = []byte("portal-drift\x00")

// weekMonday returns the Monday starting now's week, at midnight.
func weekMonday(now time.Time) time.Time {
	d := now.AddDate(0, 0, -(int(now.Weekday())+6)%7)

	return time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, now.Location())
}

// recordDrift advances user's drift state: a clean scrape disarms it, keeping
// only this week's alert on record, a first sighting only arms it, and a repeat
// confirms it. alert is true for the first confirmation in monday's week.
func recordDrift(ctx context.Context, eDB *sqlitedb.Edb, user string, drifted bool, monday time.Time) (bool, bool, error) {
	key := append(slices.Clone(driftKeyPrefix), user...)
	week := driftWeekPrefix + monday.Format(time.DateOnly)
	clean := driftCleanPrefix + week

	var confirmed, alert bool

	err := eDB.FetchAndStore(ctx, key, func(old []byte) ([]byte, error) {
		if !drifted {
			if s := string(old); s == week || s == clean {
				return []byte(clean), nil
			}

			return nil, nil
		}

		// A first sighting again. The next one confirms, and alerts only if
		// the restored week is not this one.
		if alerted, ok := strings.CutPrefix(string(old), driftCleanPrefix); ok {
			return []byte(alerted), nil
		}

		switch string(old) {
		case "":
			return []byte(driftSeen), nil
		case week:
			confirmed = true

			return old, nil
		default:
			confirmed, alert = true, true

			return []byte(week), nil
		}
	})
	if err != nil {
		return false, false, err
	}

	return confirmed, alert, nil
}

// driftMessage renders the alert. Subjects only, never grades: the findings
// name what failed to match, which is all a parent needs to act.
func driftMessage(user string, findings []string, monday time.Time) msgtypes.Message {
	shown := findings[:min(len(findings), driftMaxFindings)]
	checks := strings.Join(shown, "; ")

	if extra := len(findings) - len(shown); extra > 0 {
		checks += fmt.Sprintf(" (i još %d)", extra)
	}

	return msgtypes.Message{
		Code:         msgtypes.PortalDrift,
		Username:     user,
		Subject:      driftSubject,
		Descriptions: []string{"Upozorenje", "Provjere"},
		Fields:       []string{driftWarning, checks},
		Timestamp:    monday,
	}
}

// sendDriftAlerts turns this cycle's drift findings into alerts. A user whose
// scrape failed has no verdict and keeps their state. A confirmed drift fails
// the run every cycle, so -t and cron runs report it, but reaches the
// messengers at most once a week. Like the digest it bypasses dedup.
func sendDriftAlerts(ctx context.Context, eDB *sqlitedb.Edb, sr *seedRun, gradesMsg chan<- msgtypes.Message, now time.Time) {
	monday := weekMonday(now)

	for _, user := range sr.users {
		if ctx.Err() != nil {
			return
		}

		if sr.failed.has(user) {
			continue
		}

		findings := sr.failed.driftOf(user)

		confirmed, alert, err := recordDrift(ctx, eDB, user, len(findings) > 0, monday)
		if err != nil {
			if ctx.Err() != nil {
				return
			}

			logger.Error().Msgf("Unable to record portal drift state for %v: %v", user, err)
			exitWithError.Store(true)

			continue
		}

		if !confirmed {
			continue
		}

		logger.Error().Msgf("Portal drift confirmed for %v on two consecutive scrapes: %v", user, strings.Join(findings, "; "))
		exitWithError.Store(true)

		if !alert {
			continue
		}

		msg := driftMessage(user, findings, monday)

		// ponytail: not in the claim's transaction, like the digest: a kill in
		// between loses this alert, and the next confirmation is a week away.
		if err := queue.StoreOutbox(ctx, eDB, msg); err != nil {
			logger.Error().Msgf("%v: %v", queue.ErrQueueing, err)
		}

		gradesMsg <- msg
	}
}
