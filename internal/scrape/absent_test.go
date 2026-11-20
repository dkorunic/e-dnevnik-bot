// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package scrape

import (
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
)

// absentLegend is /absent's legend as captured live (2026-11), counts aside.
func absentLegend(excused, unexcused, pending, other int) string {
	cell := func(colour, label string, n int) string {
		return `<div class="cell"><i class="icon-circle ` + colour + `"></i><div class="absent-summary-data"><span>` +
			label + `</span><span>` + strconv.Itoa(n) + `</span></div></div>`
	}

	return `<div class="absent-legend-table"><div class="row">` +
		cell("green", "Opravdani", excused) + cell("red", "Neopravdani", unexcused) +
		cell("dark", "Čeka odluku", pending) + cell("gray", "Ukupno", excused+unexcused+pending) +
		cell("gold", "Ostalo", other) + `</div></div>`
}

// absentDay is one day's table, in the live markup: the date lives in the
// data-action-* attributes the month arrows filter on, the header repeats it.
func absentDay(day, month string, rows ...string) string {
	return `<div class="flex-table small absent-table hide" aria-label="AbsentTable" data-action-id="` + month +
		`" data-action-day="` + day + `"><div class="row header first"><div class="cell">` + day + `. ` + month +
		`</div></div>` + strings.Join(rows, "") + `</div>`
}

func absentRow(hour, subject, reason, colour string) string {
	return `<div class="row"><div class="cell"><span>` + hour + `</span></div><div class="box"><div class="cell"><span>` +
		subject + `</span></div><div class="cell"><span> ` + reason + ` </span></div></div><div class="cell">` +
		`<i class=" icon-circle ` + colour + ` "></i></div></div>`
}

func absentPage(legend string, days ...string) string {
	return `<div class="content-wrapper"><div class="content">` + legend +
		`<div class="absent-charts hide"></div><div class="before-after-month"></div>` +
		`<div class="table-wrapper-calendar"></div><div class="table-wrapper">` + strings.Join(days, "") +
		`</div></div></div>`
}

func runAbsences(t *testing.T, raw string, multiClass bool, year string) ([]msgtypes.Message, []string) {
	t.Helper()

	ch := make(chan msgtypes.Message, 64)

	mismatches, err := parseAbsences(t.Context(), ch, "u", []byte(raw), multiClass, "6.a", year)
	if err != nil {
		t.Fatalf("parseAbsences() = %v", err)
	}

	close(ch)

	return collect(ch), mismatches
}

// The live page as captured, one competition hour on 16 October.
func TestParseAbsencesLiveCapture(t *testing.T) {
	t.Parallel()

	raw := absentPage(absentLegend(0, 0, 0, 1), absentDay("16", "Listopad", absentRow("6", "Hrvatski jezik", "Natjecanje", "gold")))

	got, mismatches := runAbsences(t, raw, false, "26/27")
	if len(mismatches) != 0 {
		t.Errorf("mismatches = %v, want none", mismatches)
	}

	want := msgtypes.Message{
		Code:         msgtypes.Absence,
		Username:     "u",
		Subject:      "Hrvatski jezik",
		Class:        "6.a",
		Descriptions: []string{"Datum", "Sat", "Razlog", "Status"},
		Fields:       []string{"16.10.2026.", "6", "Natjecanje", "Ostalo"},
		Timestamp:    time.Date(2026, time.October, 16, 0, 0, 0, 0, time.UTC),
	}

	if len(got) != 1 || !equalMsg(got[0], want) {
		t.Fatalf("parseAbsences() = %+v, want %+v", got, want)
	}
}

// Every day table is read whatever month the page shows, January lands in the
// school year's second calendar year, and each status takes the legend's label.
func TestParseAbsencesWholeYear(t *testing.T) {
	t.Parallel()

	raw := absentPage(absentLegend(2, 1, 1, 0),
		absentDay("30", "Rujan", absentRow("1", "Matematika", "Bolest", "green"), absentRow("2", "Priroda", "Bolest", "green")),
		absentDay("14", "Siječanj", absentRow("3", "Povijest", "", "red")),
		absentDay("29", "Veljača", absentRow("4", "Glazbena kultura", "", "black")))

	got, mismatches := runAbsences(t, raw, true, "2027./2028.")
	if len(mismatches) != 0 {
		t.Errorf("mismatches = %v, want none", mismatches)
	}

	var rows []string
	for _, m := range got {
		rows = append(rows, m.Subject+"|"+strings.Join(m.Fields, "|"))
	}

	want := []string{
		"Matematika / 6.a|30.09.2027.|1|Bolest|Opravdani",
		"Priroda / 6.a|30.09.2027.|2|Bolest|Opravdani",
		"Povijest / 6.a|14.01.2028.|3||Neopravdani",
		"Glazbena kultura / 6.a|29.02.2028.|4||Čeka odluku",
	}

	if !slices.Equal(rows, want) {
		t.Errorf("rows =\n%v\nwant\n%v", strings.Join(rows, "\n"), strings.Join(want, "\n"))
	}
}

// A status change is a new identity, so "pending" turning "unexcused" alerts.
func TestParseAbsencesStatusIsIdentity(t *testing.T) {
	t.Parallel()

	pending, _ := runAbsences(t, absentPage(absentLegend(0, 0, 1, 0), absentDay("3", "Studeni", absentRow("2", "Priroda", "", "black"))), false, "26/27")
	decided, _ := runAbsences(t, absentPage(absentLegend(0, 1, 0, 0), absentDay("3", "Studeni", absentRow("2", "Priroda", "", "red"))), false, "26/27")

	if len(pending) != 1 || len(decided) != 1 || slices.Equal(pending[0].Fields, decided[0].Fields) {
		t.Fatalf("pending %+v and decided %+v share Fields, want the status to change the identity", pending, decided)
	}
}

// Rows the legend counts but the reader missed are drift; the total entry,
// which no row carries, is not.
func TestParseAbsencesLegendMismatch(t *testing.T) {
	t.Parallel()

	raw := absentPage(absentLegend(2, 0, 0, 0), absentDay("30", "Rujan", absentRow("1", "Matematika", "Bolest", "green")))

	_, mismatches := runAbsences(t, raw, false, "26/27")
	if len(mismatches) != 1 || !strings.Contains(mismatches[0], "Opravdani: 2 on the portal, 1 scraped") {
		t.Errorf("mismatches = %v, want one for Opravdani", mismatches)
	}
}

// An unreadable date still alerts, with the portal's own text.
func TestParseAbsencesFailsOpenOnDate(t *testing.T) {
	t.Parallel()

	raw := absentPage(absentLegend(1, 0, 0, 0), absentDay("16", "Brumaire", absentRow("6", "Matematika", "", "green")))

	got, _ := runAbsences(t, raw, false, "26/27")
	if len(got) != 1 || got[0].Fields[0] != "16. Brumaire" || !got[0].Timestamp.IsZero() {
		t.Errorf("parseAbsences() = %+v, want the raw date and no timestamp", got)
	}
}

// No absences: the page carries the legend and no day tables.
func TestParseAbsencesNone(t *testing.T) {
	t.Parallel()

	got, mismatches := runAbsences(t, absentPage(absentLegend(0, 0, 0, 0)), false, "26/27")
	if len(got) != 0 || len(mismatches) != 0 {
		t.Errorf("parseAbsences() = %+v, %v, want nothing", got, mismatches)
	}
}

func TestSchoolYearStart(t *testing.T) {
	t.Parallel()

	for in, want := range map[string]int{"26/27": 2026, "2024/2025": 2024, "2026./2027.": 2026, " 99/00": 2099} {
		if got, ok := schoolYearStart(in); !ok || got != want {
			t.Errorf("schoolYearStart(%q) = %d, %v, want %d", in, got, ok, want)
		}
	}

	if _, ok := schoolYearStart("x/y"); ok {
		t.Error(`schoolYearStart("x/y") ok, want false`)
	}
}

// notesPage is /notes as captured live (2026-11), with the teacher anonymised.
const notesPage = `<div class="content-wrapper"><div class="content "><div class="title">Razrednik</div>` +
	`<div class="section-text"> Ana Anić <a>ana.anic@skole.hr</a><br> šk. god. 2026./2027.<br><br> ujutro: ponedjeljak, 10:40h - 11:25h<br><br> popodne: srijeda, 18:20h - 19:05h </div>` +
	`<div class="title">Bilješka razrednika</div><div class="section-text"> Bilješka razrednika nije upisana. </div>` +
	`<div class="title">Izvanškolske aktivnosti</div><div class="section-text"> engleski jezik </div>` +
	`<div class="title">Izvannastavne aktivnosti</div><div class="section-text"> debata </div>` +
	`<div class="title">Vladanje</div><div class="section-text">Učenik nema upisanih vladanja.</div>` +
	`<div class="title">Pedagoške mjere</div><div class="section-text">Učenik nema pedagoških mjera.</div></div></div>`

func TestParseNotesLiveCapture(t *testing.T) {
	t.Parallel()

	ch := make(chan msgtypes.Message, 16)
	if err := parseNotes(t.Context(), ch, "u", []byte(notesPage), "6.a"); err != nil {
		t.Fatalf("parseNotes() = %v", err)
	}

	close(ch)

	got := collect(ch)

	want := [][2]string{
		{"Razrednik / 6.a", "Ana Anić ana.anic@skole.hr\nšk. god. 2026./2027.\nujutro: ponedjeljak, 10:40h - 11:25h\npopodne: srijeda, 18:20h - 19:05h"},
		{"Bilješka razrednika / 6.a", "Bilješka razrednika nije upisana."},
		{"Izvanškolske aktivnosti / 6.a", "engleski jezik"},
		{"Izvannastavne aktivnosti / 6.a", "debata"},
		{"Vladanje / 6.a", "Učenik nema upisanih vladanja."},
		{"Pedagoške mjere / 6.a", "Učenik nema pedagoških mjera."},
	}

	if len(got) != len(want) {
		t.Fatalf("parseNotes() emitted %d, want %d: %+v", len(got), len(want), got)
	}

	for i, m := range got {
		name, _, _ := strings.Cut(want[i][0], " / ")
		if m.Code != msgtypes.Note || m.Subject != want[i][0] || m.Class != "" ||
			!slices.Equal(m.Descriptions, []string{name}) || !slices.Equal(m.Fields, []string{want[i][1]}) {
			t.Errorf("note %d = %+v, want subject %q, text %q", i, m, want[i][0], want[i][1])
		}
	}
}

// A heading without its section, or an empty section, emits nothing.
func TestParseNotesSkipsIncompleteSections(t *testing.T) {
	t.Parallel()

	raw := `<div class="content"><div class="title">Vladanje</div><div class="title">Pedagoške mjere</div>` +
		`<div class="section-text">  </div></div>`

	ch := make(chan msgtypes.Message, 4)
	if err := parseNotes(t.Context(), ch, "u", []byte(raw), "6.a"); err != nil {
		t.Fatalf("parseNotes() = %v", err)
	}

	close(ch)

	if got := collect(ch); len(got) != 0 {
		t.Errorf("parseNotes() = %+v, want nothing", got)
	}
}

func equalMsg(a, b msgtypes.Message) bool {
	return a.Code == b.Code && a.Username == b.Username && a.Subject == b.Subject && a.Class == b.Class &&
		slices.Equal(a.Descriptions, b.Descriptions) && slices.Equal(a.Fields, b.Fields) && a.Timestamp.Equal(b.Timestamp)
}

// A legend that names the row's own colour wins over the black→dark alias, so
// the portal aligning the two cannot rename every pending absence.
func TestParseAbsencesLegendOwnColourWins(t *testing.T) {
	t.Parallel()

	legend := `<div class="absent-legend-table"><div class="row"><div class="cell"><i class="icon-circle black"></i>` +
		`<div class="absent-summary-data"><span>Čeka odluku</span><span>1</span></div></div></div></div>`

	got, mismatches := runAbsences(t, absentPage(legend, absentDay("3", "Studeni", absentRow("2", "Priroda", "", "black"))), false, "26/27")
	if len(mismatches) != 0 || len(got) != 1 || got[0].Fields[3] != "Čeka odluku" {
		t.Fatalf("got %+v, mismatches %v; want one Čeka odluku row and no mismatch", got, mismatches)
	}
}

// A row colour the legend lacks is named in the findings, after the count
// mismatches, which come in a stable order.
func TestParseAbsencesUnknownColour(t *testing.T) {
	t.Parallel()

	raw := absentPage(absentLegend(1, 1, 0, 0), absentDay("3", "Studeni", absentRow("2", "Priroda", "", "blue")))

	want := []string{
		"absences Opravdani: 1 on the portal, 0 scraped",
		"absences Neopravdani: 1 on the portal, 0 scraped",
		`absences: 1 rows with colour "blue", which the legend lacks`,
	}

	for range 20 {
		got, mismatches := runAbsences(t, raw, false, "26/27")
		if len(got) != 1 || got[0].Fields[3] != "blue" {
			t.Fatalf("got %+v, want the row alerted with its colour as status", got)
		}

		if !slices.Equal(mismatches, want) {
			t.Fatalf("mismatches =\n%v\nwant\n%v", strings.Join(mismatches, "\n"), strings.Join(want, "\n"))
		}
	}
}
