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

	mailCli   *mail.Client
	mailCreds credGuard  // credentials mailCli was built from
	mailMu    sync.Mutex // guards mailCli and mailCreds
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

// Mail resends queued failures, then delivers ch to the configured recipients.
// An invalid port falls back to 587.
func Mail(ctx context.Context, eDB *sqlitedb.Edb, ch <-chan msgtypes.Message, cfg MailConfig) (err error) {
	// Panic guard; stays nil on the resend path (see recoverMessenger).
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
		// Already dedup-flagged: queue them or lose them forever.
		queueUndelivered(ctx, eDB, MailQueueName, ch)

		return err
	}

	rl := ratelimit.New(MailSendLimit, ratelimit.Per(MailWindow))

	resendQueued(ctx, eDB, MailQueueName, func(m msgtypes.Message) {
		processMail(ctx, eDB, m, cfg.To, cfg.From, cfg.Subject, rl, cfg.Retries)
	})

	// Drain fully; processMail durably queues on cancelled ctx, losing nothing.
	for g := range ch {
		inflight = &g
		processMail(ctx, eDB, g, cfg.To, cfg.From, cfg.Subject, rl, cfg.Retries)
		inflight = nil
	}

	return nil
}

// mailInit lazily builds the shared client, rebuilding when any credential
// changes (see credGuard).
func mailInit(server string, portInt int, username, password string) error {
	mailMu.Lock()
	defer mailMu.Unlock()

	port := strconv.Itoa(portInt)

	if mailCli != nil && !mailCreds.changed(server, port, username, password) {
		return nil
	}

	logger.Debug().Msg("Initializing e-mail client")

	// Mandatory STARTTLS: AUTH PLAIN must never cross cleartext.
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

	// Publish only on success, so a failed init retries next cycle.
	mailCli = cli
	mailCreds.record(server, port, username, password)

	return nil
}

// markMailPermanent stops retry-go on a non-temporary SMTP error — permanent
// 5xx, auth failure, malformed header, broken TLS — as classified by
// *mail.SendError.IsTemp. Anything else keeps its retry budget.
func markMailPermanent(err error) error {
	if err == nil {
		return nil
	}

	var sendErr *mail.SendError
	if errors.As(err, &sendErr) && !sendErr.IsTemp() {
		// Inner sentinel survives retry.Do's marker stripping.
		return retry.Unrecoverable(permanentError{err})
	}

	return err
}

// processMail delivers g as a multipart/alternative batch, one message per
// recipient, re-queueing on partial or total failure. The rate limiter is taken
// once per batch rather than per recipient: delivery shares one SMTP
// connection.
func processMail(ctx context.Context, eDB *sqlitedb.Edb, g msgtypes.Message, to []string, from, subject string, rl ratelimit.Limiter, retries uint) {
	// Capped client-side: an MTA-rejected oversize would loop in the queue
	// until MaxQueueAge.
	htmlContent := truncateHTMLBody(g.Username, g.Subject, g.Code, g.Descriptions, g.Fields, MailMaxBodyChars)
	plainContent := truncateWithEllipsis(
		format.PlainMsg(g.Username, g.Subject, g.Code, g.Descriptions, g.Fields),
		MailMaxBodyChars,
	)

	run := newRecipientRun(g)

	// One message per recipient, so a partial failure stays attributable.
	var (
		pendingMsgs []*mail.Msg
		pendingRcpt []string
	)

	for _, u := range to {
		if run.skipped(u) {
			continue
		}

		m := mail.NewMsg()

		if err := m.From(from); err != nil {
			// Message-level, not the recipient's: blaming each address in turn
			// reported one config line as a fleet of dead recipients. Nothing
			// can be served and no retry helps, so stop rather than requeue.
			logger.Error().Msgf("%v: invalid From address %q, dropping alert for %v/%v: %v",
				ErrMailSendingMessages, from, g.Username, g.Subject, err)

			return
		}

		if err := m.To(u); err != nil {
			// Never becomes valid: drop.
			logger.Error().Msgf("Invalid mail To address, permanently dropping recipient %q: %v", u, err)

			run.poison(u)

			continue
		}

		m.SetMessageID()
		m.SetDate()
		m.SetBulk()

		// Capped to avoid MTA-rejected headers.
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
		// Before rl.Take(), so shutdown is not blocked on a token.
		run.interrupt()
	default:
		rl.Take()

		delivered, err := sendMailBatch(ctx, pendingMsgs, pendingRcpt, retries)
		run.delivered(delivered...)

		switch {
		case err == nil:
			// All delivered.
		case isPermanentSendErr(err):
			// Every remaining failure is permanent: drop, don't requeue.
			undelivered := undeliveredRecipients(pendingRcpt, delivered)
			run.poison(undelivered...)

			logger.Error().Msgf("%v: permanently dropping %d recipient(s): %v",
				ErrMailSendingMessages, len(undelivered), err)
		default:
			logger.Error().Msgf("%v: %v", ErrMailSendingMessages, err)

			run.failed()
		}
	}

	run.finish(ctx, eDB, MailQueueName, g)
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

// sendMailBatch delivers msgs, parallel to rcpt, over one SMTP connection per
// attempt. Only the undelivered subset is retried — Msg.IsDelivered identifies
// it — so a partial failure never re-sends to anyone already served. Retries
// short-circuit once every remaining failure is permanent.
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

			// Keep only the failures for the next attempt.
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
				// Should not happen: undelivered with no error.
				return fmt.Errorf("%w", ErrMailSendingMessages)
			}

			// Connection-level failure: nothing was attempted, so classify
			// the aggregate error directly.
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

			// Retrying helps only if one remaining failure is transient.
			for _, m := range pendingMsgs {
				var msgErr *mail.SendError
				if !errors.As(m.SendError(), &msgErr) || msgErr.IsTemp() {
					return sendErr
				}
			}

			// Inner sentinel survives retry.Do's marker stripping.
			return retry.Unrecoverable(permanentError{sendErr})
		},
	)

	return successful, err
}
