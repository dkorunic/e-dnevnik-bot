// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package messenger

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/dkorunic/e-dnevnik-bot/internal/logger"
	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
)

// examEvent is an exam as both calendar backends insert it: one all-day entry
// on Date.
type examEvent struct {
	Date        time.Time
	ID          string
	Summary     string
	Description string
}

// examEventOf maps g to its calendar entry, reporting false for what no
// calendar should receive: non-exams, past exams and field-less exams.
//
// ID is the idempotency key for both backends — Google answers a repeat with
// 409, CalDAV with 412 — so its input bytes must never change: a new ID
// re-inserts every future exam already in a user's calendar.
func examEventOf(backend string, g msgtypes.Message) (examEvent, bool) {
	if g.Code != msgtypes.Exam {
		logger.Debug().Msgf("%v: skipping non-exam event for %v/%v (code %v)", backend, g.Username, g.Subject, g.Code)

		return examEvent{}, false
	}

	// Recomputed per call so a long-running daemon never uses a stale boundary,
	// and compared as dates rather than instants: exam timestamps are midnight-UTC
	// all-day markers, so an instant comparison would drop an exam first seen on
	// the day itself.
	if g.Timestamp.Format(time.DateOnly) < time.Now().UTC().Format(time.DateOnly) {
		logger.Info().Msgf("%v: skipping old exam event for %v/%v: %+v", backend, g.Username, g.Subject, g)

		return examEvent{}, false
	}

	if len(g.Fields) == 0 {
		logger.Warn().Msgf("%v: skipping exam event for %v/%v with no fields: %+v", backend, g.Username, g.Subject, g)

		return examEvent{}, false
	}

	// Keyed on (username, subject, date), not g.Fields, so a later edit to the
	// note on the same date is a conflict no-op that keeps the original.
	// Accepted: notes rarely change once dated.
	idHash := sha256.Sum256(fmt.Appendf(nil, "%s\x00%s\x00%s",
		g.Username, g.Subject, g.Timestamp.Format(time.DateOnly)))

	ev := examEvent{
		Date:    g.Timestamp,
		ID:      hex.EncodeToString(idHash[:]),
		Summary: g.Username + CalendarExamSep + g.Subject,
	}

	// Third field of scrape's exam layout (subject, date, note). A short
	// row — a legacy queue entry — gets no description rather than a
	// mis-picked field.
	if len(g.Fields) >= 3 {
		ev.Description = g.Fields[2]
	}

	return ev, true
}
