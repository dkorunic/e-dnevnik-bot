// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package messenger

import (
	"bytes"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jordic/goics"
)

var icalStamp = time.Date(2026, 9, 23, 14, 5, 6, 0, time.UTC)

// TestBuildICalEventGolden pins the exact wire form: CRLF endings, all-day
// VALUE=DATE with an exclusive DTEND, and TEXT escaping.
func TestBuildICalEventGolden(t *testing.T) {
	t.Parallel()

	ev := examEvent{
		Date:        time.Date(2099, 9, 24, 0, 0, 0, 0, time.UTC),
		ID:          "abc123",
		Summary:     "pero.peric - Ispit iz: Matematika",
		Description: `Pisana provjera, poglavlja 1; 2 \ 3`,
	}

	want := "BEGIN:VCALENDAR\r\n" +
		"VERSION:2.0\r\n" +
		"PRODID:-//dkorunic//e-dnevnik-bot//HR\r\n" +
		"CALSCALE:GREGORIAN\r\n" +
		"BEGIN:VEVENT\r\n" +
		"UID:abc123@e-dnevnik-bot\r\n" +
		"DTSTAMP:20260923T140506Z\r\n" +
		"DTSTART;VALUE=DATE:20990924\r\n" +
		"DTEND;VALUE=DATE:20990925\r\n" +
		"SUMMARY:pero.peric - Ispit iz: Matematika\r\n" +
		`DESCRIPTION:Pisana provjera\, poglavlja 1\; 2 \\ 3` + "\r\n" +
		"TRANSP:TRANSPARENT\r\n" +
		"END:VEVENT\r\n" +
		"END:VCALENDAR\r\n"

	if got := string(buildICalEvent(ev, icalStamp)); got != want {
		t.Errorf("buildICalEvent() =\n%q\nwant\n%q", got, want)
	}
}

// TestBuildICalEventOmitsEmptyDescription: an empty DESCRIPTION line is legal
// but some clients render it as a blank note.
func TestBuildICalEventOmitsEmptyDescription(t *testing.T) {
	t.Parallel()

	ev := examEvent{Date: time.Date(2099, 9, 24, 0, 0, 0, 0, time.UTC), ID: "abc123", Summary: "s"}

	if got := string(buildICalEvent(ev, icalStamp)); strings.Contains(got, "DESCRIPTION") {
		t.Errorf("empty description was emitted:\n%q", got)
	}
}

// TestBuildICalEventYearEnd: DTEND is exclusive, so the last day of the year
// must end on 1 January of the next.
func TestBuildICalEventYearEnd(t *testing.T) {
	t.Parallel()

	ev := examEvent{Date: time.Date(2099, 12, 31, 0, 0, 0, 0, time.UTC), ID: "x", Summary: "s"}

	if got := string(buildICalEvent(ev, icalStamp)); !strings.Contains(got, "DTEND;VALUE=DATE:21000101\r\n") {
		t.Errorf("year-end DTEND wrong:\n%q", got)
	}
}

// TestICalTextEscaper covers RFC 5545 §3.3.11. A CRLF must become a single \n,
// not two.
func TestICalTextEscaper(t *testing.T) {
	t.Parallel()

	in := "a,b;c\\d\r\ne\nf\rg"
	want := `a\,b\;c\\d\ne\nf\ng`

	if got := icalTextEscaper.Replace(in); got != want {
		t.Errorf("escape(%q) = %q, want %q", in, got, want)
	}
}

// TestWriteICalLineFolds is the regression test for goics' encoder, which folds
// after appending CRLF and emits "\r\r\n \n" whenever a fold lands between the
// two. Every length across several fold boundaries, for 1- to 4-byte runes.
func TestWriteICalLineFolds(t *testing.T) {
	t.Parallel()

	for _, unit := range []string{"a", "č", "€", "😀"} {
		for n := 1; n <= 200; n++ {
			line := "DESCRIPTION:" + strings.Repeat(unit, n)

			var b strings.Builder
			writeICalLine(&b, line)
			out := b.String()

			if !strings.HasSuffix(out, "\r\n") {
				t.Fatalf("unit %q n=%d: output does not end in CRLF: %q", unit, n, out)
			}

			physical := strings.Split(strings.TrimSuffix(out, "\r\n"), "\r\n")
			for i, p := range physical {
				if len(p) > 75 {
					t.Fatalf("unit %q n=%d: line %d is %d octets, limit 75", unit, n, i, len(p))
				}

				if strings.ContainsAny(p, "\r\n") {
					t.Fatalf("unit %q n=%d: bare CR or LF in line %d: %q", unit, n, i, p)
				}

				if !utf8.ValidString(p) {
					t.Fatalf("unit %q n=%d: line %d splits a UTF-8 sequence: %q", unit, n, i, p)
				}

				if i > 0 && !strings.HasPrefix(p, " ") {
					t.Fatalf("unit %q n=%d: continuation line %d lacks leading space: %q", unit, n, i, p)
				}
			}

			if got := strings.ReplaceAll(strings.TrimSuffix(out, "\r\n"), "\r\n ", ""); got != line {
				t.Fatalf("unit %q n=%d: unfolding does not restore the line", unit, n)
			}
		}
	}
}

// icsCapture keeps what goics decoded.
type icsCapture struct{ cal *goics.Calendar }

func (c *icsCapture) ConsumeICal(cal *goics.Calendar, _ error) error {
	c.cal = cal

	return nil
}

// TestBuildICalEventRoundTrips decodes our output with an independent parser:
// a long, folded, escaped Croatian note must come back byte-identical.
func TestBuildICalEventRoundTrips(t *testing.T) {
	t.Parallel()

	note := strings.Repeat("Pisana provjera: čćžšđ ČĆŽŠĐ, poglavlja 1; 2\nDonijeti kalkulator. ", 5)
	ev := examEvent{
		Date:        time.Date(2099, 9, 24, 0, 0, 0, 0, time.UTC),
		ID:          "abc123",
		Summary:     "pero.peric - Ispit iz: Hrvatski jezik",
		Description: note,
	}

	var capture icsCapture
	if err := goics.NewDecoder(bytes.NewReader(buildICalEvent(ev, icalStamp))).Decode(&capture); err != nil {
		t.Fatalf("goics could not decode our output: %v", err)
	}

	if len(capture.cal.Events) != 1 {
		t.Fatalf("decoded %d events, want 1", len(capture.cal.Events))
	}

	data := capture.cal.Events[0].Data

	checks := map[string]string{
		"UID":         "abc123@e-dnevnik-bot",
		"DTSTART":     "20990924",
		"DTEND":       "20990925",
		"SUMMARY":     ev.Summary,
		"DESCRIPTION": note,
	}
	for key, want := range checks {
		if data[key] == nil {
			t.Errorf("%s missing after decode", key)

			continue
		}

		if data[key].Val != want {
			t.Errorf("%s = %q, want %q", key, data[key].Val, want)
		}
	}

	if data["DTSTART"] != nil && data["DTSTART"].Params["VALUE"] != "DATE" {
		t.Errorf("DTSTART params = %v, want VALUE=DATE for an all-day event", data["DTSTART"].Params)
	}
}
