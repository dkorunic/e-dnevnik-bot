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

// icalTextEscaper escapes an RFC 5545 TEXT value (§3.3.11). "\r\n" precedes
// its parts so a Windows line ending becomes one \n, not two.
var icalTextEscaper = strings.NewReplacer(
	`\`, `\\`,
	`;`, `\;`,
	`,`, `\,`,
	"\r\n", `\n`,
	"\n", `\n`,
	"\r", `\n`,
)

// buildICalEvent renders ev as a single all-day VEVENT in a VCALENDAR. stamp
// becomes DTSTAMP; it is a parameter so the output is deterministic under test.
//
// Hand-written rather than goics' encoder, which folds after appending CRLF and
// so splits "\r\n" whenever a fold lands between the two.
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
		// Exclusive end: a single day ends on the next date.
		"DTEND;VALUE=DATE:" + ev.Date.AddDate(0, 0, 1).Format(icalDate),
		"SUMMARY:" + icalTextEscaper.Replace(ev.Summary),
	}

	if ev.Description != "" {
		lines = append(lines, "DESCRIPTION:"+icalTextEscaper.Replace(ev.Description))
	}

	// An exam should not block the day as busy.
	lines = append(lines, "TRANSP:TRANSPARENT", "END:VEVENT", "END:VCALENDAR")

	var b strings.Builder
	for _, l := range lines {
		writeICalLine(&b, l)
	}

	return []byte(b.String())
}

// writeICalLine writes line folded per RFC 5545 §3.1: at most 75 octets per
// physical line, each continuation led by one space, never inside a UTF-8
// sequence.
func writeICalLine(b *strings.Builder, line string) {
	limit := icalLineOctets

	for len(line) > limit {
		cut := limit
		for cut > 0 && !utf8.RuneStart(line[cut]) {
			cut--
		}

		// Only reachable on invalid UTF-8; a byte split beats looping forever.
		if cut == 0 {
			cut = limit
		}

		b.WriteString(line[:cut])
		b.WriteString("\r\n ")
		line = line[cut:]

		// The leading space counts toward the limit.
		limit = icalLineOctets - 1
	}

	b.WriteString(line)
	b.WriteString("\r\n")
}
