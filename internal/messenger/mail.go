// SPDX-FileCopyrightText: 2022 Dinko Korunic
// SPDX-License-Identifier: MIT

package messenger

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/textproto"
	"slices"
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

	// MailDriftSubject replaces Subject, not the per-code override, for the
	// portal-change warning: under "new grade/exam" it would be skimmed past.
	MailDriftSubject = "Upozorenje: e-Dnevnik bot možda ne prima ocjene"
	MailQueue        = "mail-queue"

	// MailImplicitTLSPort is the submissions port (RFC 8314), TLS from the first
	// byte. Every other port gets STARTTLS.
	MailImplicitTLSPort = 465

	// MailCycleLimit keeps a backlog from delaying the next poll. The ticker re-arms
	// before a cycle, so one shorter than the interval costs that poll nothing, and
	// this many messages wait 42 minutes on the limiter against a shortest interval
	// of 54 (1h less jitter). A backlog therefore drains at 15 an hour.
	MailCycleLimit = 15
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
// MailSubject. Empty counts as unset at every level. The portal-change warning
// skips Subject (see MailDriftSubject).
func (cfg MailConfig) subjectFor(code msgtypes.EventCode) string {
	if code == msgtypes.PortalDrift {
		return cmp.Or(cfg.Subjects[code], MailDriftSubject)
	}

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

	cs := cycleSender{limit: MailCycleLimit}

	retries := mailRetries{attempts: cfg.Retries}

	resendQueued(ctx, eDB, MailQueueName, func(m msgtypes.Message) {
		cs.send(ctx, eDB, MailQueueName, m, func() bool {
			return processMail(ctx, eDB, m, cfg.To, cfg.From, cfg.subjectFor(m.Code), rl, &retries)
		})
	})

	// Drain fully; processMail durably queues on cancelled ctx, losing nothing.
	for g := range ch {
		inflight = &g
		cs.send(ctx, eDB, MailQueueName, g, func() bool {
			return processMail(ctx, eDB, g, cfg.To, cfg.From, cfg.subjectFor(g.Code), rl, &retries)
		})
		inflight = nil
	}

	return sendFailureErr(eDB, MailQueueName)
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

	cli, err := newMailClient(server, portInt, portInt == MailImplicitTLSPort, username, password)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrMailDialer, err)
	}

	// Publish only on success, so a failed init retries next cycle.
	mailCli = cli
	mailCreds.record(server, port, username, password)

	return nil
}

// newMailClient builds a client that never sends AUTH PLAIN in cleartext:
// implicit TLS, or mandatory STARTTLS. They cannot substitute for each other: a
// STARTTLS client waits for a banner that an implicit-TLS port withholds until
// the handshake, so each side waits on the other until the timeout.
func newMailClient(server string, port int, implicitTLS bool, username, password string) (*mail.Client, error) {
	opts := []mail.Option{
		mail.WithPort(port),
		mail.WithSMTPAuth(mail.SMTPAuthPlain),
		mail.WithTLSPolicy(mail.TLSMandatory),
		mail.WithUsername(username),
		mail.WithPassword(password),
	}

	if implicitTLS {
		opts = append(opts, mail.WithSSL())
	}

	return mail.NewClient(server, opts...)
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

// mailRetryDelay is a test seam: the wait between attempts of one batch.
var mailRetryDelay = MailMinDelay

// mailNoAuthText is go-mail v0.8's error for a server without SMTP AUTH.
const mailNoAuthText = "server does not support SMTP AUTH"

// mailConnError marks an attempt that reached no message: the server could not
// be reached or spoke no SMTP. It keeps its retries, but one that outlives them
// stops the cycle, since each further message would spend minutes of retry delay
// failing the same way while every other messenger's next poll waits.
type mailConnError struct{ err error }

func (e mailConnError) Error() string { return e.err.Error() }
func (e mailConnError) Unwrap() error { return e.err }

// mailTempError marks an attempt the server deferred, a 4xx say. One that outlives
// its retries does not stop the cycle: a single full mailbox at the head of the
// queue would then hold back every later alert. Later deferrals that cycle get one
// attempt each instead, sparing the minutes of retry delay.
type mailTempError struct{ err error }

func (e mailTempError) Error() string { return e.err.Error() }
func (e mailTempError) Unwrap() error { return e.err }

// mailRetries is one cycle's retry budget. deferred is set once a message has
// spent its attempts on 4xx answers; connection failures keep theirs regardless.
type mailRetries struct {
	attempts uint
	deferred bool
}

// mailRecipientStatuses are the enhanced statuses (RFC 3463) that name the
// recipient's address; every 5.2.x names their mailbox. 5.1.7 and 5.1.8 are absent
// because they are about the sender's address.
var mailRecipientStatuses = []string{"5.1.1", "5.1.2", "5.1.3", "5.1.6", "5.1.10"}

// isPermanentMailSendErr reports whether se can never succeed for this recipient:
// a 5xx reply to RCPT TO whose enhanced status names them, or a message go-mail
// itself cannot address or encode.
//
// A 5xx to RCPT TO proves nothing alone: Postfix delays sender and policy
// rejections to that command by default, so without such a status it must not
// drop the alert (see isMailAccountRejected, and sendMailBatch for the rest). A
// server sending no enhanced statuses therefore queues until MaxQueueAge.
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

	status := se.EnhancedStatusCode()

	return se.Reason == mail.ErrSMTPRcptTo && se.ErrorCode() >= 500 &&
		(strings.HasPrefix(status, "5.2.") || slices.Contains(mailRecipientStatuses, status))
}

// isMailRejected reports a server's 5xx reply, as opposed to a transport failure,
// which IsTemp reads as permanent but which carries no code.
func isMailRejected(se *mail.SendError) bool {
	return !se.IsTemp() && se.ErrorCode() >= 500
}

// isMailAccountRejected reports a rejection that holds for every recipient, so a
// config error rather than one recipient's failure: a 5xx to MAIL FROM (an
// unverified sender, an exhausted quota, a blocked account), or a login the server
// wants, whichever command it answers. A 5.7.x to RCPT TO is absent because it can
// be one recipient's policy, which must not hold up everybody else's mail.
//
// A 503 is absent too: go-mail sends no RSET after a failed DATA, so a strict server
// answers the next message's MAIL FROM with one. That is the connection's state.
func isMailAccountRejected(se *mail.SendError) bool {
	if !isMailRejected(se) {
		return false
	}

	code := se.ErrorCode()

	switch se.Reason { //nolint:exhaustive // the rest cannot be told apart
	case mail.ErrSMTPMailFrom:
		return code != 503
	case mail.ErrSMTPRcptTo:
		return code == 530 || code == 534 || code == 535
	default:
		return false
	}
}

// processMail delivers g as a multipart/alternative batch, one message per
// recipient, re-queueing on partial or total failure. The rate limiter is taken
// once per batch rather than per recipient: delivery shares one SMTP
// connection.
func processMail(ctx context.Context, eDB *sqlitedb.Edb, g msgtypes.Message, to []string, from, subject string, rl ratelimit.Limiter, retries *mailRetries) bool {
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
		delivered, dead, err := sendMailBatch(ctx, pendingMsgs, pendingRcpt, *retries)
		run.delivered(delivered...)

		if len(dead) > 0 {
			run.poison(dead...)

			logger.Error().Msgf("%v: permanently dropping %d recipient(s) the server rejected", ErrMailSendingMessages, len(dead))
		}

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

			last := lastAttempt(err)

			if _, unreachable := errors.AsType[mailConnError](last); unreachable {
				err = markConfigErr(err)
			}

			if _, deferred := errors.AsType[mailTempError](last); deferred {
				retries.deferred = true
			}

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
// short-circuit once every remaining failure is permanent. It returns who was
// served, and who to drop when other failures keep the error from being permanent.
func sendMailBatch(ctx context.Context, msgs []*mail.Msg, rcpt []string, retries mailRetries) ([]string, []string, error) {
	var successful, dead []string

	pendingMsgs, pendingRcpt := msgs, rcpt

	err := retry.New(
		retry.Attempts(retries.attempts),
		retry.Context(ctx),
		retry.Delay(mailRetryDelay),
		retry.MaxDelay(2*mailRetryDelay), // backoff doubles without limit, and -r has none
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
			// the aggregate error directly. Read from this attempt's error, as
			// go-mail never clears Msg.SendError: a message that failed on an
			// earlier attempt would hide a rejected login here.
			if se, ok := errors.AsType[*mail.SendError](sendErr); !ok || se.Reason == mail.ErrConnCheck {
				// A rejected login already queues; it only needs to stop
				// retrying credentials that cannot start working.
				if isMailAuthErr(sendErr) {
					return markConfigErr(sendErr)
				}

				return mailConnError{sendErr}
			}

			deferredErr := func() error {
				if retries.deferred {
					return retry.Unrecoverable(mailTempError{sendErr})
				}

				return mailTempError{sendErr}
			}

			// Retrying helps only if one remaining failure is transient.
			unknownReach := false

			var rejected []string

			for i, m := range pendingMsgs {
				msgErr, ok := errors.AsType[*mail.SendError](m.SendError())

				switch {
				case !ok:
					return deferredErr()
				case isMailAccountRejected(msgErr):
					return markConfigErr(sendErr)
				case isPermanentMailSendErr(msgErr):
					rejected = append(rejected, pendingRcpt[i])
				case isMailRejected(msgErr):
					unknownReach = true
				default:
					return deferredErr()
				}
			}

			// Say a 5xx to DATA: this message's content, or the account. Queued,
			// and the cycle goes on. Accepted cost: content rejected for good
			// (spam, say) comes back every cycle until MaxQueueAge, since quota
			// and account blocks can arrive the same way and losing those alerts
			// is worse.
			if unknownReach {
				dead = rejected

				return retry.Unrecoverable(sendErr)
			}

			// Inner sentinel survives retry.Do's marker stripping.
			return retry.Unrecoverable(permanentError{sendErr})
		},
	)

	return successful, dead, err
}
