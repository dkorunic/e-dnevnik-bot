// @license
// Copyright (C) 2026  Dinko Korunic
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

package messenger

import (
	"context"
	"time"
	"unicode/utf8"

	"github.com/dkorunic/e-dnevnik-bot/format"
	"github.com/dkorunic/e-dnevnik-bot/msgtypes"
)

// storeTimeout bounds the shutdown-tolerant context used when persisting
// failed/pending messages to the queue after the caller's context has
// already been cancelled. Must be long enough to clear the sqlite WAL but
// short enough not to delay shutdown noticeably.
const storeTimeout = 5 * time.Second

// queueStoreCtx returns a context suitable for the post-send StoreFailedMsgs
// call. If the caller's context is still live, it is used as-is so shutdown
// requests continue to propagate. If the caller's context has already been
// cancelled (the common case when the recipient loop broke out of the send
// early), a short-lived detached context is returned so the sqlite write
// still runs — otherwise the unsent message would be silently dropped on
// shutdown. The returned cancel MUST be invoked by the caller (idempotent).
func queueStoreCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx.Err() == nil {
		return ctx, func() {}
	}

	return context.WithTimeout(context.WithoutCancel(ctx), storeTimeout)
}

// Per-platform outbound body/subject size caps. Messages exceeding these limits
// are rejected by the respective APIs, so we truncate client-side to convert a
// hard failure into a slightly-lossy delivery.
const (
	TelegramMaxMessageChars = 4096 // Telegram sendMessage text limit
	SlackMaxMessageChars    = 3000 // Slack Block Kit / chat.postMessage soft cap
	MailMaxSubjectChars     = 256  // conservative; RFC 5322 is 998 bytes per line but most MUAs show ~78 chars
	WhatsAppMaxMessageChars = 4096 // whatsmeow Conversation field; protocol hard limit is ~65 KiB but most clients truncate
	DiscordMaxEmbedChars    = 6000 // Discord sum of title + description + field names + field values + footer + author
)

// mergeSkipRecipients returns existing ∪ extras with duplicates removed while
// preserving the order of first occurrence. Used when appending newly-successful
// recipients to SkipRecipients across retries — without deduplication, a message
// that repeatedly fails for different subsets of recipients accumulates
// unbounded duplicate entries in the queue.
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

// truncateHTMLBody formats username/subject/code/descriptions/grade as an HTML
// message and, if the result exceeds maxRunes, drops trailing description/grade
// pairs until it fits. Trimming the input rather than the output preserves the
// surrounding <b>/<pre> tag balance, so Telegram's HTML parser does not reject
// the message as malformed. If even the header exceeds the budget, returns the
// header-only formatted string (Telegram will reject it but the caller will at
// least see a clear error rather than a silent truncation defect).
func truncateHTMLBody(username, subject string, code msgtypes.EventCode, descriptions, grade []string, maxRunes int) string {
	formatted := format.HTMLMsg(username, subject, code, descriptions, grade)
	if utf8.RuneCountInString(formatted) <= maxRunes {
		return formatted
	}

	n := min(len(descriptions), len(grade))
	for n > 0 {
		n--

		formatted = format.HTMLMsg(username, subject, code, descriptions[:n], grade[:n])
		if utf8.RuneCountInString(formatted) <= maxRunes {
			return formatted
		}
	}

	return formatted
}

// truncateWithEllipsis truncates a string with ellipsis at the end
// if it's longer than max runes. It returns the original string if it's
// not longer than max runes.
//
// Parameters:
//
//	s - the string to truncate
//	m - the maximum number of runes the string should have
//
// Returns:
//
//	the truncated string or the original string if it's not longer than max runes.
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
