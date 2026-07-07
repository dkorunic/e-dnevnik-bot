// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package messenger

import (
	"context"
	"net/http"
	"time"
	"unicode/utf8"

	"github.com/dkorunic/e-dnevnik-bot/internal/format"
	"github.com/dkorunic/e-dnevnik-bot/internal/logger"
	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
	"github.com/dkorunic/e-dnevnik-bot/internal/queue"
	"github.com/dkorunic/e-dnevnik-bot/internal/sqlitedb"
)

// isPermanentHTTPStatus reports whether a 4xx status will never succeed on
// retry. 408 (timeout) and 429 (rate-limit) are excluded — those are transient.
func isPermanentHTTPStatus(code int) bool {
	isClientError := code >= 400 && code < 500
	isRetriable := code == http.StatusRequestTimeout || code == http.StatusTooManyRequests

	return isClientError && !isRetriable
}

// storeTimeout bounds the detached context used to persist queue writes after caller ctx cancel.
const storeTimeout = 5 * time.Second

// queueStoreCtx yields a context for the post-send StoreFailedMsgs write: the
// live ctx as-is, or — if it is already cancelled — a short-lived detached one
// so the sqlite write still runs and the message is not lost on shutdown. The
// returned cancel MUST be invoked (idempotent).
func queueStoreCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx.Err() == nil {
		return ctx, func() {}
	}

	return context.WithTimeout(context.WithoutCancel(ctx), storeTimeout)
}

// queueUndelivered is the init-failure fallback: it drains ch into the
// failed-message queue so already-dedup-flagged events survive to the next
// cycle instead of being dropped forever (dedup guarantees they never re-fire).
// Blocking until ch closes also keeps the broadcast relay from wedging on an
// undrained listener.
func queueUndelivered(ctx context.Context, eDB *sqlitedb.Edb, queueName []byte, ch <-chan msgtypes.Message) {
	queued := 0

	for g := range ch {
		// Shutdown-tolerant: queue write must survive ctx cancel.
		sctx, scancel := queueStoreCtx(ctx)
		if err := queue.StoreFailedMsgs(sctx, eDB, queueName, g); err != nil {
			logger.Error().Msgf("%v: %v", queue.ErrQueueing, err)
		}

		scancel()

		queued++
	}

	if queued > 0 {
		logger.Warn().Msgf("Messenger %v failed to initialize; stored %v undelivered messages for retry on next run",
			string(queueName), queued)
	}
}

// Per-platform outbound size caps; we truncate client-side to turn API rejection into lossy delivery.
const (
	TelegramMaxMessageChars = 4096  // Telegram sendMessage text limit
	SlackMaxMessageChars    = 3000  // Slack Block Kit / chat.postMessage soft cap
	MailMaxSubjectChars     = 256   // conservative; RFC 5322 is 998 bytes per line but most MUAs show ~78 chars
	MailMaxBodyChars        = 65536 // safety net: well below MTA size limits but bounds runaway growth
	WhatsAppMaxMessageChars = 4096  // whatsmeow Conversation field; protocol hard limit is ~65 KiB but most clients truncate
	DiscordMaxEmbedChars    = 6000  // Discord sum of title + description + field names + field values + footer + author
)

// mergeSkipRecipients returns existing ∪ extras, dedup'd, in first-seen order.
// Dedup keeps SkipRecipients from growing unboundedly as a message re-fails for
// different recipient subsets across retries.
func mergeSkipRecipients(existing, extras []string) []string {
	if len(extras) == 0 {
		return existing
	}

	seen := make(map[string]struct{}, len(existing)+len(extras))

	out := make([]string, 0, len(existing)+len(extras))

	for _, s := range existing {
		if _, ok := seen[s]; ok {
			continue
		}

		seen[s] = struct{}{}

		out = append(out, s)
	}

	for _, s := range extras {
		if _, ok := seen[s]; ok {
			continue
		}

		seen[s] = struct{}{}

		out = append(out, s)
	}

	return out
}

// truncateHTMLBody renders an HTML message that fits within maxRunes by
// dropping trailing description/grade pairs. Trimming the input (not the
// output) keeps the <b>/<pre> tags balanced so Telegram's parser accepts it;
// an over-budget header falls back to header-only. Uses binary search over the
// pair count (O(log N) renders).
func truncateHTMLBody(username, subject string, code msgtypes.EventCode, descriptions, grade []string, maxRunes int) string {
	nMax := min(len(descriptions), len(grade))

	formatted := format.HTMLMsg(username, subject, code, descriptions[:nMax], grade[:nMax])
	if utf8.RuneCountInString(formatted) <= maxRunes {
		return formatted
	}

	// Invariant: lo is the largest pair count known to fit (-1 if none yet).
	lo, hi := -1, nMax
	for lo+1 < hi {
		mid := lo + (hi-lo)/2
		candidate := format.HTMLMsg(username, subject, code, descriptions[:mid], grade[:mid])

		if utf8.RuneCountInString(candidate) <= maxRunes {
			lo = mid
			formatted = candidate
		} else {
			hi = mid
		}
	}

	if lo < 0 {
		// Even the header exceeds the budget; best-effort return.
		return format.HTMLMsg(username, subject, code, nil, nil)
	}

	return formatted
}

// truncateWithEllipsis shortens s to at most m runes, appending "..." when it
// trims. Strings already within budget are returned unchanged.
func truncateWithEllipsis(s string, m int) string {
	if utf8.RuneCountInString(s) <= m {
		return s
	}

	count := 0
	cutoff := 0

	for i := range s {
		if count == m-3 {
			cutoff = i
		}

		count++

		if count > m {
			return s[:cutoff] + "..."
		}
	}

	return s
}
