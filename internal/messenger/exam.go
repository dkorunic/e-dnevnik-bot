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

// examEvent is an exam as both calendar backends insert it: one all-day entry.
type examEvent struct {
	Date        time.Time
	ID          string
	Summary     string
	Description string
}

// examEventOf maps g to its calendar entry, reporting false for what no
// calendar should receive: non-exams, past exams and field-less exams.
//
// ID is both backends' idempotency key (Google 409, CalDAV 412). Its input
// bytes are frozen: a new ID re-inserts every future exam already stored.
func examEventOf(backend string, g msgtypes.Message) (examEvent, bool) {
	if g.Code != msgtypes.Exam {
		logger.Debug().Msgf("%v: skipping non-exam event for %v/%v (code %v)", backend, g.Username, g.Subject, g.Code)

		return examEvent{}, false
	}

	// Dates, not instants, recomputed per call: exam timestamps are midnight-UTC
	// markers, so an instant comparison would drop an exam on its own day.
	if g.Timestamp.Format(time.DateOnly) < time.Now().UTC().Format(time.DateOnly) {
		logger.Info().Msgf("%v: skipping old exam event for %v/%v: %+v", backend, g.Username, g.Subject, g)

		return examEvent{}, false
	}

	if len(g.Fields) == 0 {
		logger.Warn().Msgf("%v: skipping exam event for %v/%v with no fields: %+v", backend, g.Username, g.Subject, g)

		return examEvent{}, false
	}

	// Not keyed on g.Fields: an edited note on the same date conflicts and the
	// original stands. Accepted — notes rarely change once dated.
	idHash := sha256.Sum256(fmt.Appendf(nil, "%s\x00%s\x00%s",
		g.Username, g.Subject, g.Timestamp.Format(time.DateOnly)))

	ev := examEvent{
		Date:    g.Timestamp,
		ID:      hex.EncodeToString(idHash[:]),
		Summary: g.Username + CalendarExamSep + g.Subject,
	}

	// scrape's exam layout is (subject, date, note); a shorter legacy queue row
	// gets no description rather than a mis-picked field.
	if len(g.Fields) >= 3 {
		ev.Description = g.Fields[2]
	}

	return ev, true
}
