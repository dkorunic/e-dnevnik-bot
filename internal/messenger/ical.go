// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package messenger

import (
	"strings"
	"time"
	"unicode/utf8"
)

const (
	CalDAVProdID    = "-//dkorunic//e-dnevnik-bot//HR"
	CalDAVUIDSuffix = "@e-dnevnik-bot"

	icalLineOctets  = 75 // RFC 5545 §3.1, CRLF excluded
	icalDate        = "20060102"
	icalDateTimeUTC = "20060102T150405Z"
)

// icalTextEscaper escapes an RFC 5545 TEXT value (§3.3.11). "\r\n" is listed
// before its parts so it becomes one \n, not two.
var icalTextEscaper = strings.NewReplacer(
	`\`, `\\`,
	`;`, `\;`,
	`,`, `\,`,
	"\r\n", `\n`,
	"\n", `\n`,
	"\r", `\n`,
)

// buildICalEvent renders ev as a one-event VCALENDAR. stamp is DTSTAMP, passed
// in so tests are deterministic.
//
// Hand-written because goics' encoder folds after appending CRLF and splits
// "\r\n" whenever a fold lands between them.
func buildICalEvent(ev examEvent, stamp time.Time) []byte {
	lines := []string{
		"BEGIN:VCALENDAR",
		"VERSION:2.0",
		"PRODID:" + CalDAVProdID,
		"CALSCALE:GREGORIAN",
		"BEGIN:VEVENT",
		"UID:" + ev.ID + CalDAVUIDSuffix,
		"DTSTAMP:" + stamp.UTC().Format(icalDateTimeUTC),
		"DTSTART;VALUE=DATE:" + ev.Date.Format(icalDate),
		// DTEND is exclusive.
		"DTEND;VALUE=DATE:" + ev.Date.AddDate(0, 0, 1).Format(icalDate),
		"SUMMARY:" + icalTextEscaper.Replace(ev.Summary),
	}

	if ev.Description != "" {
		lines = append(lines, "DESCRIPTION:"+icalTextEscaper.Replace(ev.Description))
	}

	// Don't mark the day busy.
	lines = append(lines, "TRANSP:TRANSPARENT", "END:VEVENT", "END:VCALENDAR")

	var b strings.Builder
	for _, l := range lines {
		writeICalLine(&b, l)
	}

	return []byte(b.String())
}

// writeICalLine folds line per RFC 5545 §3.1 without splitting a UTF-8
// sequence.
func writeICalLine(b *strings.Builder, line string) {
	limit := icalLineOctets

	for len(line) > limit {
		cut := limit
		for cut > 0 && !utf8.RuneStart(line[cut]) {
			cut--
		}

		// Invalid UTF-8 only; a byte split beats looping forever.
		if cut == 0 {
			cut = limit
		}

		b.WriteString(line[:cut])
		b.WriteString("\r\n ")
		line = line[cut:]

		// The continuation's leading space counts.
		limit = icalLineOctets - 1
	}

	b.WriteString(line)
	b.WriteString("\r\n")
}
