// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package scrape

import (
	"testing"
	"time"

	"github.com/dkorunic/e-dnevnik-bot/internal/fetch"
)

// The budget spans every request of a scrape, so -r 1 must not leave it one
// request's timeout; above the floor, -r still buys more time.
func TestScrapeBudget(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		attempts, want uint
	}{
		{attempts: 1, want: scrapeMinBudgetRequests},
		{attempts: 3, want: scrapeMinBudgetRequests},
		{attempts: scrapeMinBudgetRequests, want: scrapeMinBudgetRequests},
		{attempts: scrapeMaxAttempts, want: scrapeMaxAttempts},
	} {
		want := time.Duration(tt.want) * fetch.Timeout
		if got := scrapeBudget(tt.attempts); got != want {
			t.Errorf("scrapeBudget(%d) = %v, want %v", tt.attempts, got, want)
		}
	}
}
