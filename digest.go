// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package main

import (
	"cmp"
	"context"
	"slices"
	"sync"
	"time"

	"github.com/dkorunic/e-dnevnik-bot/internal/config"
	"github.com/dkorunic/e-dnevnik-bot/internal/logger"
	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
	"github.com/dkorunic/e-dnevnik-bot/internal/sqlitedb"
)

const (
	digestDateFormat = "2.1."
	digestRangeSep   = " – "
	digestNoteSep    = " – "
)

// digestKeyPrefix keys each user's last digested week; cannot collide with hashes.
var digestKeyPrefix = []byte("digest-week\x00")

// hrWeekday holds Croatian weekday abbreviations, indexed by time.Weekday.
var hrWeekday = [...]string{"ned", "pon", "uto", "sri", "čet", "pet", "sub"}

// userSet collects users whose scrape failed this cycle; safe for concurrent use.
type userSet struct {
	m  map[string]struct{}
	mu sync.Mutex
}

func (s *userSet) add(user string) {
	if s == nil {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.m == nil {
		s.m = make(map[string]struct{})
	}

	s.m[user] = struct{}{}
}

func (s *userSet) has(user string) bool {
	if s == nil {
		return false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	_, ok := s.m[user]

	return ok
}

// digestRun is one cycle's weekly-digest job; nil disables it.
type digestRun struct {
	now    time.Time // zero means time.Now()
	failed *userSet
	users  []string
	day    time.Weekday
	hour   int
}

// digestWeek returns the Monday of the week after the latest day@hour.
// Anchoring on the past send point lets any later poll catch up.
func digestWeek(now time.Time, day time.Weekday, hour int) time.Time {
	y, m, d := now.Date()
	loc := now.Location()

	since := (int(now.Weekday()) - int(day) + 7) % 7

	// time.Date normalises days, so DST cannot shift midnight.
	sendAt := time.Date(y, m, d-since, hour, 0, 0, 0, loc)
	if sendAt.After(now) {
		sendAt = sendAt.AddDate(0, 0, -7)
	}

	// A Monday send day covers the following week, not the one it opens.
	toMonday := (8 - int(day)) % 7
	if toMonday == 0 {
		toMonday = 7
	}

	return time.Date(sendAt.Year(), sendAt.Month(), sendAt.Day()+toMonday, 0, 0, 0, 0, loc)
}

// buildDigest lists user's exams from now's date to monday's Sunday; false if none.
// Dates compare as strings: timestamps are midnight-UTC markers, like examEventOf.
func buildDigest(user string, exams []msgtypes.Message, monday, now time.Time) (msgtypes.Message, bool) {
	sunday := monday.AddDate(0, 0, 6)

	// A late digest omits days already past.
	start := monday
	if now.Format(time.DateOnly) > monday.Format(time.DateOnly) {
		start = now
	}

	from, to := start.Format(time.DateOnly), sunday.Format(time.DateOnly)

	type row struct{ date, subject, note string }

	rows := make([]row, 0, len(exams))

	for _, g := range exams {
		date := g.Timestamp.Format(time.DateOnly)
		if date < from || date > to {
			continue
		}

		r := row{date: date, subject: g.Subject}

		// scrape's exam layout is (subject, date, note).
		if len(g.Fields) >= 3 {
			r.note = g.Fields[2]
		}

		rows = append(rows, r)
	}

	if len(rows) == 0 {
		return msgtypes.Message{}, false
	}

	slices.SortFunc(rows, func(a, b row) int {
		return cmp.Or(cmp.Compare(a.date, b.date), cmp.Compare(a.subject, b.subject), cmp.Compare(a.note, b.note))
	})

	// Folds identical rows the portal lists twice.
	rows = slices.Compact(rows)

	msg := msgtypes.Message{
		Code:         msgtypes.ExamDigest,
		Username:     user,
		Subject:      start.Format(digestDateFormat) + digestRangeSep + sunday.Format(digestDateFormat),
		Timestamp:    monday,
		Descriptions: make([]string, 0, len(rows)),
		Fields:       make([]string, 0, len(rows)),
	}

	for _, r := range rows {
		// Cannot fail: formatted with the same layout.
		d, _ := time.Parse(time.DateOnly, r.date)

		value := r.subject
		if r.note != "" {
			value += digestNoteSep + r.note
		}

		msg.Descriptions = append(msg.Descriptions, hrWeekday[d.Weekday()]+" "+d.Format(digestDateFormat))
		msg.Fields = append(msg.Fields, value)
	}

	return msg, true
}

// claimDigestWeek marks user's week handled; false if already claimed.
// Claim before handoff: a failed post-send claim would resend every cycle.
func claimDigestWeek(ctx context.Context, eDB *sqlitedb.Edb, user string, monday time.Time) (bool, error) {
	week := []byte(monday.Format(time.DateOnly))
	key := append(slices.Clone(digestKeyPrefix), user...)

	claimed := false

	err := eDB.FetchAndStore(ctx, key, func(old []byte) ([]byte, error) {
		if slices.Equal(old, week) {
			return old, nil
		}

		claimed = true

		return week, nil
	})

	return claimed && err == nil, err
}

// sendDigestsFn is a test seam: reaching recoverDedup from here needs a panic.
var sendDigestsFn = sendDigests

// sendDigests emits due digests; failed scrapes stay unclaimed for retry.
// Empty weeks are claimed silently.
func sendDigests(ctx context.Context, eDB *sqlitedb.Edb, dg *digestRun, exams map[string][]msgtypes.Message, gradesMsg chan<- msgtypes.Message) {
	now := dg.now
	if now.IsZero() {
		now = time.Now()
	}

	monday := digestWeek(now, dg.day, dg.hour)

	for _, user := range dg.users {
		if ctx.Err() != nil {
			return
		}

		if dg.failed.has(user) {
			// Debug: the scrape failure itself is already logged, every cycle.
			logger.Debug().Msgf("Weekly exam digest for %v postponed: scraping failed this cycle", user)

			continue
		}

		claimed, err := claimDigestWeek(ctx, eDB, user, monday)
		if err != nil {
			logger.Error().Msgf("Unable to record weekly exam digest for %v: %v", user, err)
			exitWithError.Store(true)

			continue
		}

		if !claimed {
			continue
		}

		msg, ok := buildDigest(user, exams[user], monday, now)
		if !ok {
			logger.Info().Msgf("No exams for %v in the week of %v, skipping weekly digest", user, monday.Format(time.DateOnly))

			continue
		}

		logger.Info().Msgf("Weekly exam digest for %v: %d exams", user, len(msg.Fields))

		gradesMsg <- msg
	}
}

// usernames lists configured users in config order.
func usernames(users []config.User) []string {
	out := make([]string, 0, len(users))
	for _, u := range users {
		out = append(out, u.Username)
	}

	return out
}
