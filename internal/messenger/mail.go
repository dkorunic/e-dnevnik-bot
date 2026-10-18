// SPDX-FileCopyrightText: 2022 Dinko Korunic
// SPDX-License-Identifier: MIT

package messenger

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/textproto"
	"strconv"
	"strings"
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
	Subjects map[msgtypes.EventCode]string // per-code override of Subject
	To       []string
	Retries  uint
}

// subjectFor resolves the subject: per-code override, then Subject, then
// MailSubject. Empty counts as unset at every level.
func (cfg MailConfig) subjectFor(code msgtypes.EventCode) string {
	return cmp.Or(cfg.Subjects[code], cfg.Subject, MailSubject)
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

	var cs cycleSender

	resendQueued(ctx, eDB, MailQueueName, func(m msgtypes.Message) {
		cs.send(ctx, eDB, MailQueueName, m, func() bool {
			return processMail(ctx, eDB, m, cfg.To, cfg.From, cfg.subjectFor(m.Code), rl, cfg.Retries)
		})
	})

	// Drain fully; processMail durably queues on cancelled ctx, losing nothing.
	for g := range ch {
		inflight = &g
		cs.send(ctx, eDB, MailQueueName, g, func() bool {
			return processMail(ctx, eDB, g, cfg.To, cfg.From, cfg.subjectFor(g.Code), rl, cfg.Retries)
		})
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

// isMailAuthErr reports an SMTP login rejection (530 auth required, 534 auth
// mechanism too weak, 535 bad credentials). It fails the dial, before any
// message is attempted, and applies to every recipient: a config error, queued
// rather than dropped.
//
// go-mail's own login setup failures are config errors too, as no retry can
// change them. With SMTPAuthPlain fixed (see mailInit) only two are reachable:
// ErrPlainAuthNotSupported, and a server advertising no AUTH at all, which
// go-mail reports as an unnamed fmt.Errorf — hence the text match.
func isMailAuthErr(err error) bool {
	if errors.Is(err, mail.ErrPlainAuthNotSupported) || strings.Contains(err.Error(), mailNoAuthText) {
		return true
	}

	var tpErr *textproto.Error
	if !errors.As(err, &tpErr) {
		return false
	}

	return tpErr.Code == 530 || tpErr.Code == 534 || tpErr.Code == 535
}

// mailNoAuthText is go-mail v0.8's error for a server without SMTP AUTH.
const mailNoAuthText = "server does not support SMTP AUTH"

// markMailPermanent stops retry-go on a SendError isPermanentMailSendErr
// condemns. Anything else keeps its retry budget.
func markMailPermanent(err error) error {
	if err == nil {
		return nil
	}

	if sendErr, ok := errors.AsType[*mail.SendError](err); ok && isPermanentMailSendErr(sendErr) {
		// Inner sentinel survives retry.Do's marker stripping.
		return retry.Unrecoverable(permanentError{err})
	}

	return err
}

// isPermanentMailSendErr reports whether se can never succeed on retry: a 5xx
// reply, or a message go-mail itself cannot address or encode.
//
// IsTemp alone cannot decide it. go-mail judges temp by a leading '4' in the
// error text, so a transport failure — EOF, a reset, a timeout, a connection
// dropped after dial or between messages — reads as permanent and would
// poison-drop alerts dedup has already flagged. Such errors carry no server
// code, which is what tells them apart from a real rejection.
func isPermanentMailSendErr(se *mail.SendError) bool {
	if se.IsTemp() {
		return false
	}

	if se.Reason == mail.ErrGetSender || se.Reason == mail.ErrGetRcpts || se.Reason == mail.ErrNoUnencoded {
		return true
	}

	return se.ErrorCode() >= 500
}

// processMail delivers g as a multipart/alternative batch, one message per
// recipient, re-queueing on partial or total failure. The rate limiter is taken
// once per batch rather than per recipient: delivery shares one SMTP
// connection.
func processMail(ctx context.Context, eDB *sqlitedb.Edb, g msgtypes.Message, to []string, from, subject string, rl ratelimit.Limiter, retries uint) bool {
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
			// reported one config line as a fleet of dead recipients. A config
			// error: queued until fixed, and the cycle stops sending.
			logger.Error().Msgf("%v: invalid From address %q, queueing alert for %v/%v: %v",
				ErrMailSendingMessages, from, g.Username, g.Subject, err)

			run.failed(markConfigErr(err))

			break
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
		m.Subject(truncateWithEllipsis(cmp.Or(subject, MailSubject), MailMaxSubjectChars))

		m.SetBodyString(mail.TypeTextPlain, plainContent)
		m.AddAlternativeString(mail.TypeTextHTML, htmlContent)

		pendingMsgs = append(pendingMsgs, m)
		pendingRcpt = append(pendingRcpt, u)
	}

	switch {
	case len(pendingMsgs) == 0:
		// Nothing left to send after skips/poison.
	case !takeCtx(ctx, rl):
		// Mail's token can be minutes away; shutdown must not wait it out.
		run.interrupt()
	default:
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

			run.failed(err)
		}
	}

	return run.finish(ctx, eDB, MailQueueName, g)
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
				// A rejected login already queues (it is no SendError, so
				// markMailPermanent reads it as transient); it only needs to
				// stop retrying credentials that cannot start working.
				if isMailAuthErr(sendErr) {
					return markConfigErr(sendErr)
				}

				return markMailPermanent(sendErr)
			}

			// Retrying helps only if one remaining failure is transient.
			for _, m := range pendingMsgs {
				msgErr, ok := errors.AsType[*mail.SendError](m.SendError())
				if !ok || !isPermanentMailSendErr(msgErr) {
					return sendErr
				}
			}

			// Inner sentinel survives retry.Do's marker stripping.
			return retry.Unrecoverable(permanentError{sendErr})
		},
	)

	return successful, err
}
