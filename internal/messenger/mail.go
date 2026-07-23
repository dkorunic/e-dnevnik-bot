// SPDX-FileCopyrightText: 2022 Dinko Korunic
// SPDX-License-Identifier: MIT

package messenger

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/avast/retry-go/v5"
	"github.com/dkorunic/e-dnevnik-bot/internal/format"
	"github.com/dkorunic/e-dnevnik-bot/internal/logger"
	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
	"github.com/dkorunic/e-dnevnik-bot/internal/queue"
	"github.com/dkorunic/e-dnevnik-bot/internal/sqlitedb"
	"github.com/dkorunic/e-dnevnik-bot/internal/version"
	mail "github.com/wneessen/go-mail"
	"go.uber.org/ratelimit"
)

const (
	MailSendLimit = 20 // 20 emails per 1 hour
	MailWindow    = 1 * time.Hour
	MailMinDelay  = MailWindow / MailSendLimit
	MailSubject   = "Nova ocjena/ispit iz e-Dnevnika"
	MailQueue     = "mail-queue"
)

var (
	ErrMailInvalidPort     = errors.New("invalid or missing SMTP port, will try with default 587/tcp")
	ErrMailDialer          = errors.New("failed to create mail delivery client")
	ErrMailSendingMessages = errors.New("error sending mail messages")

	MailQueueName = []byte(MailQueue)
	MailVersion   = version.ReadVersion("github.com/wneessen/go-mail")

	mailCli *mail.Client
	mailMu  sync.Mutex // guards mailCli initialisation
)

// MailConfig holds the per-messenger settings for the e-mail backend.
type MailConfig struct {
	Server   string
	Port     string
	Username string
	Password string
	From     string
	Subject  string
	To       []string
	Retries  uint
}

// Mail resends any queued failures, then delivers live messages from ch to the
// configured recipients. An invalid port falls back to 587.
func Mail(ctx context.Context, eDB *sqlitedb.Edb, ch <-chan msgtypes.Message, cfg MailConfig) (err error) {
	// A send-path panic must drain ch and degrade, not crash the process.
	// inflight: nil on the resend path (queue row persists); set only while draining ch.
	var inflight *msgtypes.Message

	defer func() {
		if r := recover(); r != nil {
			err = recoverMessenger(ctx, eDB, MailQueueName, ch, r, inflight)
		}
	}()

	logger.Debug().Msgf("Started e-mail messenger (%v)", MailVersion)

	portInt, err := strconv.Atoi(cfg.Port)
	if err != nil {
		logger.Warn().Msgf("%v: %v", ErrMailInvalidPort, cfg.Port)

		portInt = 587
	}

	if err = mailInit(cfg.Server, portInt, cfg.Username, cfg.Password); err != nil {
		// Events are already dedup-flagged; queue them or they are lost forever.
		queueUndelivered(ctx, eDB, MailQueueName, ch)

		return err
	}

	rl := ratelimit.New(MailSendLimit, ratelimit.Per(MailWindow))

	// Resend queued failures first. Rows are only removed after processing, so
	// a crash mid-loop re-delivers instead of losing; on shutdown, unprocessed
	// rows simply stay queued for the next run.
	for _, q := range queue.FetchFailedMsgs(ctx, eDB, MailQueueName) {
		if ctx.Err() != nil {
			break
		}

		processMail(ctx, eDB, q.Msg, cfg.To, cfg.From, cfg.Subject, rl, cfg.Retries)

		// Failures were re-queued by processMail; drop the original row.
		queue.Dequeue(ctx, eDB, q.Key)
	}

	// Drain fully; processMail durably queues on cancelled ctx, losing nothing.
	for g := range ch {
		inflight = &g
		processMail(ctx, eDB, g, cfg.To, cfg.From, cfg.Subject, rl, cfg.Retries)
		inflight = nil
	}

	return nil
}

// mailInit initializes the mail client once, reusing it across all subsequent calls.
func mailInit(server string, portInt int, username, password string) error {
	mailMu.Lock()
	defer mailMu.Unlock()

	if mailCli == nil {
		logger.Debug().Msg("Initializing e-mail client")

		// Mandatory STARTTLS: AUTH PLAIN must never traverse cleartext.
		cli, err := mail.NewClient(server,
			mail.WithPort(portInt),
			mail.WithSMTPAuth(mail.SMTPAuthPlain),
			mail.WithTLSPolicy(mail.TLSMandatory),
			mail.WithUsername(username),
			mail.WithPassword(password),
		)
		if err != nil {
			return fmt.Errorf("%w: %w", ErrMailDialer, err)
		}

		// Publish only on full success so a failed init is retried next cycle.
		mailCli = cli
	}

	return nil
}

// markMailPermanent marks non-temporary SMTP errors (permanent 5xx, auth
// failure, malformed header, broken TLS) as unrecoverable so retry-go stops
// retrying. Classification comes from *mail.SendError.IsTemp; anything else
// keeps its normal retry budget.
func markMailPermanent(err error) error {
	if err == nil {
		return nil
	}

	var sendErr *mail.SendError
	if errors.As(err, &sendErr) && !sendErr.IsTemp() {
		// permanentError inside: survives retry.Do's marker stripping.
		return retry.Unrecoverable(permanentError{err})
	}

	return err
}

// processMail formats g as a multipart/alternative (text + HTML) message per
// recipient and delivers the batch, re-queueing on partial or total failure
// with an accurate SkipRecipients set. Recipients already in SkipRecipients
// are omitted. The rate limiter is taken once per alert batch, not per
// recipient, since delivery shares one SMTP connection.
func processMail(ctx context.Context, eDB *sqlitedb.Edb, g msgtypes.Message, to []string, from, subject string, rl ratelimit.Limiter, retries uint) {
	// Cap body size client-side; an MTA-rejected oversize would otherwise
	// loop indefinitely in the failed-message queue.
	htmlContent := truncateHTMLBody(g.Username, g.Subject, g.Code, g.Descriptions, g.Fields, MailMaxBodyChars)
	plainContent := truncateWithEllipsis(
		format.PlainMsg(g.Username, g.Subject, g.Code, g.Descriptions, g.Fields),
		MailMaxBodyChars,
	)

	skipSet := make(map[string]struct{}, len(g.SkipRecipients))
	for _, r := range g.SkipRecipients {
		skipSet[r] = struct{}{}
	}

	var successfulIDs []string

	// Permanently-failed recipients: skipped on retry, never requeued.
	var poisonedIDs []string

	anyFailed := false
	// Tracks incomplete batches so shutdown-cancelled sends get re-queued, not dropped.
	allProcessed := true

	// Build one message per recipient upfront so partial failures stay
	// attributable per recipient.
	var (
		pendingMsgs []*mail.Msg
		pendingRcpt []string
	)

	for _, u := range to {
		if _, skip := skipSet[u]; skip {
			continue
		}

		m := mail.NewMsg()

		if err := m.From(from); err != nil {
			// Malformed From is permanent: the message can never be sent, so drop.
			logger.Error().Msgf("Invalid mail From address %v, permanently dropping recipient %q: %v", from, u, err)

			poisonedIDs = append(poisonedIDs, u)

			continue
		}

		if err := m.To(u); err != nil {
			// Malformed To never becomes valid: drop.
			logger.Error().Msgf("Invalid mail To address, permanently dropping recipient %q: %v", u, err)

			poisonedIDs = append(poisonedIDs, u)

			continue
		}

		m.SetMessageID()
		m.SetDate()
		m.SetBulk()

		// Cap subject length to avoid MTA-rejected headers.
		if subject != "" {
			m.Subject(truncateWithEllipsis(subject, MailMaxSubjectChars))
		} else {
			m.Subject(MailSubject)
		}

		m.SetBodyString(mail.TypeTextPlain, plainContent)
		m.AddAlternativeString(mail.TypeTextHTML, htmlContent)

		pendingMsgs = append(pendingMsgs, m)
		pendingRcpt = append(pendingRcpt, u)
	}

	switch {
	case len(pendingMsgs) == 0:
		// Nothing left to send after skips/poison.
	case ctx.Err() != nil:
		// Check before rl.Take() so shutdown is not blocked on a token.
		allProcessed = false
	default:
		rl.Take()

		delivered, err := sendMailBatch(ctx, pendingMsgs, pendingRcpt, retries)
		successfulIDs = append(successfulIDs, delivered...)

		switch {
		case err == nil:
			// All delivered.
		case isPermanentSendErr(err):
			// All undelivered failed permanently (e.g. SMTP 5xx): drop, don't requeue.
			undelivered := undeliveredRecipients(pendingRcpt, delivered)
			poisonedIDs = append(poisonedIDs, undelivered...)

			logger.Error().Msgf("%v: permanently dropping %d recipient(s): %v",
				ErrMailSendingMessages, len(undelivered), err)
		default:
			logger.Error().Msgf("%v: %v", ErrMailSendingMessages, err)

			anyFailed = true
		}
	}

	if anyFailed || !allProcessed {
		// Skip successful and poisoned recipients on retry; dedup bounds growth.
		g.SkipRecipients = mergeSkipRecipients(g.SkipRecipients, append(successfulIDs, poisonedIDs...))

		// Shutdown-tolerant: queue write must survive ctx cancel.
		sctx, scancel := queueStoreCtx(ctx)
		if err := queue.StoreFailedMsgs(sctx, eDB, MailQueueName, g); err != nil {
			logger.Error().Msgf("%v: %v", queue.ErrQueueing, err)
		}

		scancel()
	}
}

// undeliveredRecipients returns the entries of rcpt not in delivered, in order.
func undeliveredRecipients(rcpt, delivered []string) []string {
	deliveredSet := make(map[string]struct{}, len(delivered))
	for _, d := range delivered {
		deliveredSet[d] = struct{}{}
	}

	var out []string

	for _, r := range rcpt {
		if _, ok := deliveredSet[r]; !ok {
			out = append(out, r)
		}
	}

	return out
}

// sendMailBatch delivers msgs (parallel to rcpt) over one SMTP connection per
// attempt, retrying only the undelivered subset — identified via
// Msg.IsDelivered — so a partial failure never re-sends to already-delivered
// recipients. Retries short-circuit once every remaining failure is permanent.
// Returns the delivered recipients and the last send error, if any remained.
func sendMailBatch(ctx context.Context, msgs []*mail.Msg, rcpt []string, retries uint) ([]string, error) {
	var successful []string

	pendingMsgs, pendingRcpt := msgs, rcpt

	err := retry.New(
		retry.Attempts(retries),
		retry.Context(ctx),
		retry.Delay(MailMinDelay),
	).Do(
		func() error {
			sendErr := mailCli.DialAndSendWithContext(ctx, pendingMsgs...)

			// Winnow delivered messages so only failures are retried.
			var stillMsgs []*mail.Msg

			var stillRcpt []string

			for i, m := range pendingMsgs {
				if m.IsDelivered() {
					successful = append(successful, pendingRcpt[i])

					continue
				}

				stillMsgs = append(stillMsgs, m)
				stillRcpt = append(stillRcpt, pendingRcpt[i])
			}

			pendingMsgs, pendingRcpt = stillMsgs, stillRcpt

			if len(pendingMsgs) == 0 {
				return nil
			}

			if sendErr == nil {
				// Defensive: undelivered without error should not happen.
				return fmt.Errorf("%w", ErrMailSendingMessages)
			}

			// Dial/connection-level failure: no message was attempted,
			// classify the aggregate error directly.
			attempted := false

			for _, m := range pendingMsgs {
				if m.HasSendError() {
					attempted = true

					break
				}
			}

			if !attempted {
				return markMailPermanent(sendErr)
			}

			// Retry only helps if at least one remaining failure is
			// transient; short-circuit when all are permanent.
			for _, m := range pendingMsgs {
				var msgErr *mail.SendError
				if !errors.As(m.SendError(), &msgErr) || msgErr.IsTemp() {
					return sendErr
				}
			}

			// permanentError inside: survives retry.Do's marker stripping.
			return retry.Unrecoverable(permanentError{sendErr})
		},
	)

	return successful, err
}
