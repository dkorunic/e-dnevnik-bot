// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package messenger

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/dkorunic/e-dnevnik-bot/internal/format"
	"github.com/dkorunic/e-dnevnik-bot/internal/logger"
	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
	"github.com/dkorunic/e-dnevnik-bot/internal/queue"
	"github.com/dkorunic/e-dnevnik-bot/internal/sqlitedb"
)

// configRewriteMu serialises LoadConfig→SaveConfig cycles so concurrent
// rewrites — WhatsApp group resolution and a Telegram chat-ID remap in the same
// cycle, say — cannot lose each other's changes.
var configRewriteMu sync.Mutex

// ErrMessengerPanic flags the run as failed after a recovered send panic.
var ErrMessengerPanic = errors.New("messenger panicked")

// isPermanentHTTPStatus reports whether a 4xx will never succeed on retry.
// 408 and 429 are transient.
func isPermanentHTTPStatus(code int) bool {
	isClientError := code >= 400 && code < 500
	isRetriable := code == http.StatusRequestTimeout || code == http.StatusTooManyRequests

	return isClientError && !isRetriable
}

// permanentError survives retry.Do unwrapping: retry-go v5 strips the outer
// Unrecoverable marker on return, so this inner sentinel is what keeps errors.As
// working after the retry loop.
type permanentError struct{ err error }

func (e permanentError) Error() string { return e.err.Error() }
func (e permanentError) Unwrap() error { return e.err }

// isPermanentSendErr reports whether a markNamePermanent helper classified err
// as permanent, before or after retry.Do. Such failures — blocked bot, deleted
// chat, 4xx — must be poison-dropped, never requeued: they would re-attempt
// every cycle until MaxQueueAge.
func isPermanentSendErr(err error) bool {
	var perr permanentError

	return errors.As(err, &perr)
}

// QueueAccepts reports whether anything will read g back. Both calendar
// backends take exams alone, and Calendar's deferred stub never reads its
// queue, so anything else sits unconsumable until MaxQueueAge.
//
// Keyed on the queue rather than passed in: the overflow spill, init failure and
// panic drain must all agree, and a parameter is something a new route forgets.
func QueueAccepts(queueName []byte, g msgtypes.Message) bool {
	if bytes.Equal(queueName, CalendarQueueName) || bytes.Equal(queueName, CalDAVQueueName) {
		return g.Code == msgtypes.Exam
	}

	return true
}

// storeTimeout bounds queue writes detached after the caller's ctx is cancelled.
const storeTimeout = 5 * time.Second

// queueStoreCtx yields a context for the post-send queue write: the live one,
// or a short-lived detached one if it is already cancelled, so a shutdown
// mid-send still persists the message. The cancel must always be invoked.
func queueStoreCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx.Err() == nil {
		return ctx, func() {}
	}

	return context.WithTimeout(context.WithoutCancel(ctx), storeTimeout)
}

// queueUndelivered drains ch to the queue so already-flagged events survive to
// the next cycle; dedup never re-fires them. Blocking until ch closes also lets
// msgSend's fan-out close and wait cleanly. A store failure is only logged —
// with one durable store there is no fallback.
func queueUndelivered(ctx context.Context, eDB *sqlitedb.Edb, queueName []byte, ch <-chan msgtypes.Message) {
	queued := 0
	skipped := 0

	for g := range ch {
		if !QueueAccepts(queueName, g) {
			skipped++

			continue
		}

		// Must survive ctx cancel.
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

	if skipped > 0 {
		logger.Debug().Msgf("Messenger %v does not deliver %v of the undelivered messages; dropped rather than queued",
			string(queueName), skipped)
	}
}

// recoverMessenger isolates a panicking backend from the process and preserves
// its backlog in the queue.
//
// inflight must be nil between messages and on the resend path, where the queue
// row still exists and passing it would duplicate. Recipients already served
// before a mid-send panic may see a duplicate — at-least-once, matching the
// queue's crash semantics.
func recoverMessenger(ctx context.Context, eDB *sqlitedb.Edb, queueName []byte, ch <-chan msgtypes.Message, r any, inflight *msgtypes.Message) error {
	logger.Error().Msgf("Messenger %v panicked, draining undelivered messages to queue for retry: %v",
		string(queueName), r)

	// Off the channel but not yet in the queue: orphaned by the panic.
	if inflight != nil && QueueAccepts(queueName, *inflight) {
		sctx, scancel := queueStoreCtx(ctx)
		if err := queue.StoreFailedMsgs(sctx, eDB, queueName, *inflight); err != nil {
			logger.Error().Msgf("%v: %v", queue.ErrQueueing, err)
		}

		scancel()
	}

	queueUndelivered(ctx, eDB, queueName, ch)

	return fmt.Errorf("%w: %v", ErrMessengerPanic, r)
}

// Per-platform outbound caps. Truncating client-side turns an API rejection
// into lossy delivery.
const (
	TelegramMaxMessageChars = 4096  // Telegram sendMessage text limit
	SlackMaxMessageChars    = 3000  // Slack Block Kit / chat.postMessage soft cap
	MailMaxSubjectChars     = 256   // conservative; RFC 5322 is 998 bytes per line but most MUAs show ~78 chars
	MailMaxBodyChars        = 65536 // safety net: well below MTA size limits but bounds runaway growth
	WhatsAppMaxMessageChars = 4096  // whatsmeow Conversation field; protocol hard limit is ~65 KiB but most clients truncate
	DiscordMaxEmbedChars    = 6000  // Discord sum of title + description + field names + field values + footer + author
)

// mergeSkipRecipients returns existing ∪ extras in first-seen order. Dedup is
// what bounds growth as a message re-fails for different subsets.
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

// resendQueued replays queueName through process, dropping each row only once
// its outcome is durable.
//
// The ordering is the contract that makes delivery at-least-once: rows stay in
// place, process re-queues its own failures as fresh rows, and only then is the
// original dequeued. Dequeuing first loses an alert on a crash; never dequeuing
// redelivers it every cycle.
//
// A cancelled ctx leaves the remainder for the next cycle. Callers must keep
// their inflight pointer nil throughout — the row is still in the database, so
// recording it would duplicate it.
func resendQueued(ctx context.Context, eDB *sqlitedb.Edb, queueName []byte, process func(msgtypes.Message)) {
	for _, q := range queue.FetchFailedMsgs(ctx, eDB, queueName) {
		if ctx.Err() != nil {
			break
		}

		process(q.Msg)

		// process re-queued its failures as new rows; drop the original.
		queue.Dequeue(ctx, eDB, q.Key)
	}
}

// recipientRun accumulates one message's per-recipient outcome and owns the
// decision to requeue it.
//
// These rules only hold if every backend applies them identically: a poisoned
// recipient joins SkipRecipients but must not itself requeue (that retries until
// MaxQueueAge); a delivered one joins it so a retry cannot double-send; and the
// queue write must survive ctx cancel. Five hand-copied blocks were five places
// to drift — cf. cellValues in internal/scrape.
//
// The messenger drives this, not the reverse: send loop, error classification
// and rate limiting stay per-backend.
type recipientRun struct {
	skip       map[string]struct{}
	successful []string
	poisoned   []string

	// A transient failure: requeue so the recipient is retried next cycle.
	anyFailed bool

	// Cancelled before every recipient was attempted: requeue even though
	// nothing failed.
	interrupted bool
}

// newRecipientRun seeds the skip set from recipients an earlier attempt
// already resolved.
func newRecipientRun(g msgtypes.Message) *recipientRun {
	skip := make(map[string]struct{}, len(g.SkipRecipients))
	for _, r := range g.SkipRecipients {
		skip[r] = struct{}{}
	}

	return &recipientRun{skip: skip}
}

// skipped reports whether an earlier attempt already resolved id.
func (r *recipientRun) skipped(id string) bool {
	_, ok := r.skip[id]

	return ok
}

// delivered records recipients this attempt reached.
func (r *recipientRun) delivered(ids ...string) {
	r.successful = append(r.successful, ids...)
}

// poison records permanent failures — blocked bot, deleted chat, 4xx. Dropped
// loudly, never requeued.
func (r *recipientRun) poison(ids ...string) {
	r.poisoned = append(r.poisoned, ids...)
}

// failed records a transient failure, which requeues the message.
func (r *recipientRun) failed() {
	r.anyFailed = true
}

// interrupt records that recipients were left unattempted.
func (r *recipientRun) interrupt() {
	r.interrupted = true
}

// resolved returns every recipient not to attempt again, delivered and poisoned
// alike, as a fresh slice — a caller's append must not alias the accumulators.
func (r *recipientRun) resolved() []string {
	out := make([]string, 0, len(r.successful)+len(r.poisoned))
	out = append(out, r.successful...)
	out = append(out, r.poisoned...)

	return out
}

// finish requeues g when anything is left to retry, merging resolved recipients
// into SkipRecipients first. A run with neither a transient failure nor an
// interruption is complete — an all-poisoned one included — and writes nothing.
func (r *recipientRun) finish(ctx context.Context, eDB *sqlitedb.Edb, queueName []byte, g msgtypes.Message) {
	if !r.anyFailed && !r.interrupted {
		return
	}

	// Neither is attempted again; dedup bounds growth.
	g.SkipRecipients = mergeSkipRecipients(g.SkipRecipients, r.resolved())

	// Must survive ctx cancel.
	sctx, scancel := queueStoreCtx(ctx)
	defer scancel()

	if err := queue.StoreFailedMsgs(sctx, eDB, queueName, g); err != nil {
		logger.Error().Msgf("%v: %v", queue.ErrQueueing, err)
	}
}

// bodyRenderer is satisfied by format.HTMLMsg and format.MarkupMsg.
type bodyRenderer func(username, subject string, code msgtypes.EventCode, descriptions, grade []string) string

// truncateHTMLBody fits an HTML message within maxRunes.
func truncateHTMLBody(username, subject string, code msgtypes.EventCode, descriptions, grade []string, maxRunes int) string {
	return truncateRendered(format.HTMLMsg, username, subject, code, descriptions, grade, maxRunes)
}

// truncateRendered fits a message within maxRunes by dropping trailing
// description/grade pairs and re-rendering.
//
// Trimming the input, not the output, is the point: a cut in the rendered string
// splits whatever it lands in — a <b>/<pre> tag Telegram rejects, a Slack
// fence's closing ```, an &-entity arriving as "&am". Dropping pairs can only
// lose whole rows. An over-budget header falls back to header-only and may still
// exceed maxRunes, since nothing smaller renders. O(log N) renders.
func truncateRendered(render bodyRenderer, username, subject string, code msgtypes.EventCode, descriptions, grade []string, maxRunes int) string {
	nMax := min(len(descriptions), len(grade))

	formatted := render(username, subject, code, descriptions[:nMax], grade[:nMax])
	if utf8.RuneCountInString(formatted) <= maxRunes {
		return formatted
	}

	// lo is the largest pair count known to fit, -1 until one does.
	lo, hi := -1, nMax
	for lo+1 < hi {
		mid := lo + (hi-lo)/2
		candidate := render(username, subject, code, descriptions[:mid], grade[:mid])

		if utf8.RuneCountInString(candidate) <= maxRunes {
			lo = mid
			formatted = candidate
		} else {
			hi = mid
		}
	}

	if lo < 0 {
		// Even the header is over budget; best effort.
		return render(username, subject, code, nil, nil)
	}

	return formatted
}

// truncateWithEllipsis shortens s to at most m runes, appending "..." when it
// trims. Below the ellipsis width (no caller does this) it truncates plainly, so
// the result never exceeds m runes.
func truncateWithEllipsis(s string, m int) string {
	if utf8.RuneCountInString(s) <= m {
		return s
	}

	if m < 3 {
		if m <= 0 {
			return ""
		}

		return string([]rune(s)[:m])
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
