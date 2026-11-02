// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package messenger

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/avast/retry-go/v5"
	"github.com/dkorunic/e-dnevnik-bot/internal/config"
	"github.com/dkorunic/e-dnevnik-bot/internal/format"
	"github.com/dkorunic/e-dnevnik-bot/internal/logger"
	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
	"github.com/dkorunic/e-dnevnik-bot/internal/queue"
	"github.com/dkorunic/e-dnevnik-bot/internal/sqlitedb"
	"go.mau.fi/whatsmeow"
	"go.uber.org/ratelimit"
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

// configError marks a failure that applies to every recipient and message — a
// rejected token, login or URL, or an exhausted quota. Like permanentError it survives retry.Do's marker
// stripping, but its message is queued, not dropped: the alerts are already
// dedup-flagged, and fixing the config and restarting delivers them.
type configError struct{ err error }

func (e configError) Error() string { return e.err.Error() }
func (e configError) Unwrap() error { return e.err }

// markConfigErr stops retry-go on err and classifies it as a config error.
func markConfigErr(err error) error {
	return retry.Unrecoverable(configError{err})
}

// isConfigSendErr reports whether err was classified by markConfigErr, before
// or after retry.Do.
func isConfigSendErr(err error) bool {
	_, ok := errors.AsType[configError](err)

	return ok
}

// cycleSender stops a messenger from repeating a rejected credential for the
// rest of a cycle. Once one message reports a config error, every later one is
// queued unsent: retrying a bad SMTP password per message is how an account
// gets locked out, and a revoked token only collects more rejections. Lives
// for one entry-point call, so the next cycle tries the credentials afresh.
//
// limit, when set, also caps how many messages one cycle sends, the rest waiting
// in the queue for the next: mail's rate limiter sleeps minutes per message, and
// the poll cycle waits for it.
type cycleSender struct {
	limit   int
	sent    int
	tripped bool
}

// send runs process for g — which reports whether it hit a config error —
// unless an earlier message already did, in which case g is queued as is.
func (c *cycleSender) send(ctx context.Context, eDB *sqlitedb.Edb, queueName []byte, g msgtypes.Message, process func() bool) {
	full := c.limit > 0 && c.sent >= c.limit
	if full && c.sent == c.limit {
		c.sent++ // once

		logger.Info().Msgf("Messenger %v: sent its %d messages for this cycle; queueing the rest for the next",
			string(queueName), c.limit)
	}

	if c.tripped || full {
		// A calendar queue would only discard a non-exam on resend.
		if QueueAccepts(queueName, g) {
			storeQueued(ctx, eDB, queueName, g)
		}

		return
	}

	c.sent++

	if process() {
		c.tripped = true

		logger.Warn().Msgf("Messenger %v: a failure affecting every message (credentials, configuration, quota or an unreachable server); queueing the rest of this cycle unsent",
			string(queueName))
	}
}

// ErrSendFailed is an entry point's report that a message it took did not fully
// land: it was dropped for a recipient, or queued for another try. Without it a
// bad password latched nothing, so -t and a cron run exited 0 having sent nothing.
var ErrSendFailed = errors.New("some messages were not delivered, see the errors above")

// sendFailureKey is per database, opened afresh each cycle, so a failure counts
// for the cycle it happened in; and per queue, so one messenger's failure does not
// read as another's.
type sendFailureKey struct {
	eDB   *sqlitedb.Edb
	queue string
}

var (
	sendFailuresMu sync.Mutex
	sendFailures   = map[sendFailureKey]struct{}{}
)

// recordSendFailure notes a send that did not land, for the entry point's return.
// A shutdown cutting it short does not count: SIGTERM must still exit clean.
func recordSendFailure(eDB *sqlitedb.Edb, queueName []byte, err error) {
	if errors.Is(err, context.Canceled) {
		return
	}

	sendFailuresMu.Lock()
	defer sendFailuresMu.Unlock()

	sendFailures[sendFailureKey{eDB, string(queueName)}] = struct{}{}
}

// sendFailureErr returns ErrSendFailed if a send on queueName failed this cycle.
func sendFailureErr(eDB *sqlitedb.Edb, queueName []byte) error {
	sendFailuresMu.Lock()
	defer sendFailuresMu.Unlock()

	if _, failed := sendFailures[sendFailureKey{eDB, string(queueName)}]; failed {
		return fmt.Errorf("%w: %s", ErrSendFailed, queueName)
	}

	return nil
}

// ForgetSendFailures drops eDB's entries, which pin its handle: a new one opens
// every poll cycle. Call it once its messengers have returned.
func ForgetSendFailures(eDB *sqlitedb.Edb) {
	sendFailuresMu.Lock()
	defer sendFailuresMu.Unlock()

	maps.DeleteFunc(sendFailures, func(k sendFailureKey, _ struct{}) bool { return k.eDB == eDB })
}

// isUnreachable reports a timeout or a failed dial: nothing the server said about
// this message, so the next one would wait out the same timeouts while every
// messenger's next poll waits too. A reset mid-request stays per message, and
// whatsmeow's ErrNotConnected too: it clears on a reconnect, within the retries.
//
// Of a retry.Do aggregate only the last attempt counts: it is what the next
// message would meet, and errors.As would answer for whichever came first.
func isUnreachable(err error) bool {
	if agg, ok := errors.AsType[retry.Error](err); ok {
		err = agg.LastError()
	}

	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}

	if errors.Is(err, whatsmeow.ErrIQTimedOut) || errors.Is(err, whatsmeow.ErrMessageTimedOut) {
		return true
	}

	if ne, ok := errors.AsType[net.Error](err); ok && ne.Timeout() {
		return true
	}

	oe, ok := errors.AsType[*net.OpError](err)

	return ok && oe.Op == "dial"
}

// stopsCycle reports a failure that would repeat for every remaining message.
func stopsCycle(err error) bool {
	return isConfigSendErr(err) || isUnreachable(err)
}

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

// takeCtx waits for a rate-limit token, reporting false once ctx is cancelled.
// ratelimit.Limiter.Take cannot be interrupted, and mail's 20/h limit can sleep
// minutes per message — past a container's or systemd's stop timeout, so the
// SIGKILL would lose every buffered, already dedup-flagged message. An
// abandoned Take finishes in its goroutine and spends a token nobody uses,
// which is harmless on the way out.
func takeCtx(ctx context.Context, rl ratelimit.Limiter) bool {
	if ctx.Err() != nil {
		return false
	}

	done := make(chan struct{})

	go func() {
		rl.Take()
		close(done)
	}()

	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	}
}

// storeQueued queues g for the next cycle. The write survives ctx cancel (see
// queue.StoreFailedMsgs); a failure is only logged — with one durable store
// there is no fallback. queue counts it, which is how resendQueued keeps the row
// it came from.
func storeQueued(ctx context.Context, eDB *sqlitedb.Edb, queueName []byte, g msgtypes.Message) {
	if err := queue.StoreFailedMsgs(ctx, eDB, queueName, g); err != nil {
		logger.Error().Msgf("%v: %v", queue.ErrQueueing, err)
	}
}

// rewriteConfig applies mutate to confFile as it is on disk now and saves the
// result when mutate reports a change. Serialised by configRewriteMu, so two
// rewrites in one cycle cannot lose each other's changes. Best effort: who
// labels the log line, and the return reports whether the file was written.
func rewriteConfig(confFile, who string, mutate func(*config.TomlConfig) bool) bool {
	configRewriteMu.Lock()
	defer configRewriteMu.Unlock()

	if !isWriteable(confFile) {
		logger.Warn().Msgf("%v: cannot update configuration, %q is not writable", who, confFile)

		return false
	}

	// Non-validating: LoadConfig's fail-fast validators would os.Exit this
	// goroutine on a broken mid-run edit.
	cfg, err := config.LoadConfigRaw(confFile)
	if err != nil {
		logger.Error().Msgf("%v: unable to load configuration: %v", who, err)

		return false
	}

	if !mutate(&cfg) {
		return false
	}

	if err := config.SaveConfig(confFile, cfg); err != nil {
		logger.Error().Msgf("%v: unable to save configuration: %v", who, err)

		return false
	}

	return true
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

		storeQueued(ctx, eDB, queueName, g)

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
		storeQueued(ctx, eDB, queueName, *inflight)
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

// queuedExpired reports whether g announces something already over: an exam sat,
// or a digest whose week has ended. A row can wait out a long outage, and chat
// messengers would otherwise deliver these as news. Dates, not instants, as in
// examEventOf. An undated row stays: nothing says it is over.
func queuedExpired(g msgtypes.Message, now time.Time) bool {
	if g.Timestamp.IsZero() {
		return false
	}

	today := now.UTC().Format(time.DateOnly)

	switch g.Code {
	case msgtypes.Exam:
		return g.Timestamp.Format(time.DateOnly) < today
	case msgtypes.ExamDigest:
		// Timestamp is the week's Monday.
		return g.Timestamp.AddDate(0, 0, 6).Format(time.DateOnly) < today
	case msgtypes.Grade, msgtypes.Reading, msgtypes.FinalGrade, msgtypes.NationalExam:
	}

	return false
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

		if queuedExpired(q.Msg, time.Now()) {
			logger.Info().Msgf("Dropping queued message for %v/%v from queue %v: its date has passed",
				q.Msg.Username, q.Msg.Subject, string(queueName))

			queue.Dequeue(ctx, eDB, q.Key)

			continue
		}

		failures := queue.StoreFailureCount(eDB, queueName)

		process(q.Msg)

		// A requeue that did not land leaves this row the only copy. Recipients
		// reached just now may see a duplicate, which beats a lost alert.
		if queue.StoreFailureCount(eDB, queueName) != failures {
			continue
		}

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

	// Something did not land for a reason other than shutdown; reported to the
	// entry point through recordSendFailure.
	failure bool

	// Cancelled before every recipient was attempted: requeue even though
	// nothing failed.
	interrupted bool

	// A failure bad for every recipient (see stopsCycle), so the recipient loop
	// stops and finish reports it to cycleSender.
	configFailed bool
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
	r.failure = true
}

// failed records a non-permanent failure, which requeues the message. It
// reports whether err stops the cycle (see stopsCycle), in which case the caller
// must stop its recipient loop: the rest would only repeat the same failure.
func (r *recipientRun) failed(err error) bool {
	r.anyFailed = true

	if !errors.Is(err, context.Canceled) {
		r.failure = true
	}

	if stopsCycle(err) {
		r.configFailed = true
	}

	return r.configFailed
}

// interrupt records that recipients were left unattempted.
func (r *recipientRun) interrupt() {
	r.interrupted = true
}

// unreached records recipients this attempt could not even name, which requeues
// the message like an interruption but, unlike one, counts as a failure.
func (r *recipientRun) unreached() {
	r.interrupted = true
	r.failure = true
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
func (r *recipientRun) finish(ctx context.Context, eDB *sqlitedb.Edb, queueName []byte, g msgtypes.Message) bool {
	if r.failure {
		recordSendFailure(eDB, queueName, nil)
	}

	if !r.anyFailed && !r.interrupted {
		return r.configFailed
	}

	// Neither is attempted again; dedup bounds growth.
	g.SkipRecipients = mergeSkipRecipients(g.SkipRecipients, r.resolved())

	storeQueued(ctx, eDB, queueName, g)

	return r.configFailed
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
