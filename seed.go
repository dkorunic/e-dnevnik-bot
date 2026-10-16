// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package main

import (
	"context"

	"github.com/dkorunic/e-dnevnik-bot/internal/logger"
	"github.com/dkorunic/e-dnevnik-bot/internal/sqlitedb"
)

var (
	// seedKeyPrefix marks a user whose existing events have been recorded
	// silently, under seeded\x00<user>.
	seedKeyPrefix = []byte("seeded\x00")

	// seedMigratedKey records that per-user markers are in use. Its absence
	// on a database with rows means one written before them.
	seedMigratedKey = []byte("seeded-migrated")
)

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
func loadSeeded(ctx context.Context, eDB *sqlitedb.Edb, sr *seedRun) map[string]bool {
	seeded := make(map[string]bool, len(sr.users))

	rows, err := eDB.ScanPrefix(ctx, seedKeyPrefix)
	if err != nil {
		// Fall back to the old whole-database rule rather than guess.
		logger.Error().Msgf("Unable to read first-run markers, falling back to database presence: %v", err)

		for _, u := range sr.users {
			seeded[u] = eDB.Existing()
		}

		return seeded
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

		return seeded
	}

	if len(migrated) > 0 {
		return seeded
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

	if !complete {
		return seeded
	}

	if err := eDB.Put(ctx, seedMigratedKey, []byte{1}); err != nil {
		logger.Error().Msgf("Unable to record first-run marker migration: %v", err)
	}

	return seeded
}

// markSeeded records every unseeded user whose scrape succeeded this cycle. A
// failed or cancelled scrape stays unseeded, so its next run is silent again.
func markSeeded(ctx context.Context, eDB *sqlitedb.Edb, sr *seedRun, seeded map[string]bool) {
	if ctx.Err() != nil {
		return
	}

	for _, u := range sr.users {
		if seeded[u] || sr.failed.has(u) {
			continue
		}

		if putSeedMarker(ctx, eDB, u) {
			logger.Info().Msgf("First run for %v recorded; new-event alerts start next cycle", u)
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
