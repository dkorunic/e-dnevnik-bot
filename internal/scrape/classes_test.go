// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package scrape

import (
	"errors"
	"testing"
)

// An authenticated page with no classes must not read as a complete scrape:
// that seeds a new user with nothing, so the next cycle alerts their history.
func TestActiveClassesReportsNone(t *testing.T) {
	t.Parallel()

	if _, err := activeClasses("u@skole.hr", []byte(`<html><body></body></html>`)); !errors.Is(err, ErrNoActiveClasses) {
		t.Errorf("activeClasses() error = %v, want ErrNoActiveClasses", err)
	}
}

func TestActiveClassesPassesClassesThrough(t *testing.T) {
	t.Parallel()

	raw := []byte(`<div class="student-list"><div class="classes"><div class="class-menu-vertical">` +
		`<div class="class-info" data-action-id="42"></div></div></div></div>`)

	classes, err := activeClasses("u@skole.hr", raw)
	if err != nil || len(classes) != 1 || classes[0].ID != "42" {
		t.Errorf("activeClasses() = %+v, %v, want class 42", classes, err)
	}
}
