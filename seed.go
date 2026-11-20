// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"slices"
	"strconv"

	"github.com/dkorunic/e-dnevnik-bot/internal/logger"
	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
	"github.com/dkorunic/e-dnevnik-bot/internal/sqlitedb"
)

var (
	// seedKeyPrefix marks a user whose existing events have been recorded
	// silently, under seeded\x00<user>.
	seedKeyPrefix = []byte("seeded\x00")

	// seedMigratedKey records that per-user markers are in use. Its absence
	// on a database with rows means one written before them.
	seedMigratedKey = []byte("seeded-migrated")

	// codeSeedKeyPrefix marks a user whose events of one lateSeedCodes code
	// have been recorded silently, under seeded-code\x00<code>\x00<user>.
	codeSeedKeyPrefix = []byte("seeded-code\x00")
)

// lateSeedCodes are event codes added after installs were already seeded. Each
// gets its own silent first run per user, or the upgrade would alert a user's
// whole history of them at once: every absence of the school year, every note
// section including the "nothing recorded" placeholders. Append a new scraped
// code here; never remove one, or its markers stop being read.
var lateSeedCodes = []msgtypes.EventCode{msgtypes.Absence, msgtypes.Note}

// codeSeedName is a codeSeeded key, and the suffix of its database key. The
// ordinal is stable (see msgtypes.EventCode).
func codeSeedName(code msgtypes.EventCode, user string) string {
	return strconv.Itoa(int(code)) + "\x00" + user
}

// isFirstRun reports whether user's events of code only seed this cycle: the
// user's own first run, or a late code's.
func isFirstRun(seeded, codeSeeded map[string]bool, user string, code msgtypes.EventCode) bool {
	if !seeded[user] {
		return true
	}

	return slices.Contains(lateSeedCodes, code) && !codeSeeded[codeSeedName(code, user)]
}

// loadCodeSeeded reads the per-code markers, keyed by codeSeedName. On a read
// error it returns nil: every late code then counts as a first run, which only
// delays its alerts, and markSeeded writes no marker on a guess.
func loadCodeSeeded(ctx context.Context, eDB *sqlitedb.Edb) map[string]bool {
	rows, err := eDB.ScanPrefix(ctx, codeSeedKeyPrefix)
	if err != nil {
		logger.Error().Msgf("Unable to read per-event first-run markers, recording absences and notes silently: %v", err)

		return nil
	}

	seeded := make(map[string]bool, len(rows))
	for _, r := range rows {
		seeded[string(r.Key[len(codeSeedKeyPrefix):])] = true
	}

	return seeded
}

// seedRun is what per-user first-run seeding needs from the cycle.
//
// Seeding used to be decided for the whole database — "does any row exist" —
// so a first run that scraped user A but not B (a portal timeout, a wrong
// password, SIGTERM) left B unseeded, and the next cycle alerted B's entire
// history. Adding a [[user]] to an existing install did the same.
type seedRun struct {
	failed *userSet // users whose scrape failed this cycle
	users  []string // configured usernames
}

// loadSeeded reports, per configured user, whether their first run is done.
//
// A database from before per-user markers is migrated once: its history is
// every configured user's seed, so all are marked, or the upgrade would cost a
// cycle of alerts. A [[user]] added in that same upgrade is marked too and may
// alert its history once — the one case the migration cannot tell apart.
//
// ok is false when a fresh database could not record that markers are in use.
// The caller must then flag nothing this cycle: rows without the migration key
// are what a pre-marker database looks like, so the next cycle would mark every
// user seeded and one whose first scrape failed would alert its whole history.
// Nothing alerts on a fresh database anyway, and unflagged events re-scrape.
func loadSeeded(ctx context.Context, eDB *sqlitedb.Edb, sr *seedRun) (map[string]bool, bool) {
	seeded := make(map[string]bool, len(sr.users))

	rows, err := eDB.ScanPrefix(ctx, seedKeyPrefix)
	if err != nil {
		// Fall back to the old whole-database rule rather than guess.
		logger.Error().Msgf("Unable to read first-run markers, falling back to database presence: %v", err)

		for _, u := range sr.users {
			seeded[u] = eDB.Existing()
		}

		return seeded, eDB.Existing()
	}

	for _, r := range rows {
		seeded[string(r.Key[len(seedKeyPrefix):])] = true
	}

	migrated, err := eDB.ScanPrefix(ctx, seedMigratedKey)
	if err != nil {
		// Unknown migration state: an unmarked user here may be a pre-marker
		// one, and guessing "first run" flags their new events unsent.
		logger.Error().Msgf("Unable to read first-run marker migration state, falling back to database presence: %v", err)

		for _, u := range sr.users {
			seeded[u] = seeded[u] || eDB.Existing()
		}

		return seeded, eDB.Existing()
	}

	if len(migrated) > 0 {
		return seeded, true
	}

	// The migration key commits the migration, so it is written only once
	// every marker is: a missing marker behind it reads as a first run.
	complete := true

	if eDB.Existing() {
		for _, u := range sr.users {
			if !putSeedMarker(ctx, eDB, u) {
				complete = false
			}

			seeded[u] = true
		}
	}

	// Only reachable on a pre-marker database, which retries next cycle.
	if !complete {
		return seeded, true
	}

	if err := eDB.Put(ctx, seedMigratedKey, []byte{1}); err != nil {
		logger.Error().Msgf("Unable to record first-run marker migration: %v", err)

		return seeded, eDB.Existing()
	}

	return seeded, true
}

// markSeeded records every unseeded user whose scrape succeeded this cycle. A
// failed or cancelled scrape stays unseeded, so its next run is silent again.
//
// codeSeeded nil means the per-code markers could not be read, so none is
// written: a marker must follow a silent run, not a guess.
func markSeeded(ctx context.Context, eDB *sqlitedb.Edb, sr *seedRun, seeded, codeSeeded map[string]bool) {
	if ctx.Err() != nil {
		return
	}

	for _, u := range sr.users {
		if sr.failed.has(u) {
			continue
		}

		if !seeded[u] && putSeedMarker(ctx, eDB, u) {
			logger.Info().Msgf("First run for %v recorded; new-event alerts start next cycle", u)
		}

		if codeSeeded == nil {
			continue
		}

		for _, code := range lateSeedCodes {
			// A skipped page saw none of the code's history: seeding on it
			// would alert all of it next cycle.
			name := codeSeedName(code, u)
			if codeSeeded[name] || sr.failed.skippedCode(u, code) {
				continue
			}

			key := append(append([]byte{}, codeSeedKeyPrefix...), name...)

			if err := eDB.Put(ctx, key, []byte{1}); err != nil {
				logger.Error().Msgf("Unable to record first-run marker for %v, code %v: %v", u, code, err)

				continue
			}

			if seeded[u] {
				logger.Info().Msgf("Existing events of code %v recorded for %v; their alerts start next cycle", code, u)
			}
		}
	}
}

// putSeedMarker writes u's marker, with no TTL so the TTL sweep keeps it, and
// reports whether it landed.
func putSeedMarker(ctx context.Context, eDB *sqlitedb.Edb, u string) bool {
	key := append(append([]byte{}, seedKeyPrefix...), u...)

	if err := eDB.Put(ctx, key, []byte{1}); err != nil {
		logger.Error().Msgf("Unable to record first-run marker for %v: %v", u, err)

		return false
	}

	return true
}
