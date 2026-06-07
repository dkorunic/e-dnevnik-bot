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

// Mail sends messages through the mail service to the specified recipients.
//
// It takes the following parameters:
// - ctx: the context.Context object for cancellation and timeouts.
// - eDB: the database instance for checking and storing failed messages.
// - ch: the channel from which to receive messages.
// - cfg: the e-mail messenger configuration (server, auth, sender, recipients, retries).
//
// The function processes all failed messages and new messages, sending them
// through the mail service. It uses a rate limiter to control the message
// sending rate and supports retry attempts for sending failures. It logs
// invalid ports and sets a default port if necessary.
func Mail(ctx context.Context, eDB *sqlitedb.Edb, ch <-chan msgtypes.Message, cfg MailConfig) error {
	logger.Debug().Msgf("Started e-mail messenger (%v)", MailVersion)

	portInt, err := strconv.Atoi(cfg.Port)
	if err != nil {
		logger.Warn().Msgf("%v: %v", ErrMailInvalidPort, cfg.Port)

		portInt = 587
	}

	if err = mailInit(cfg.Server, portInt, cfg.Username, cfg.Password); err != nil {
		return err
	}

	rl := ratelimit.New(MailSendLimit, ratelimit.Per(MailWindow))

	// Drain queued failures first; re-queue tail on shutdown.
	failedMsgs := queue.FetchFailedMsgs(ctx, eDB, MailQueueName)
	for i, g := range failedMsgs {
		if ctx.Err() != nil {
			queue.RequeueMsgs(ctx, eDB, MailQueueName, failedMsgs[i:])

			return ctx.Err()
		}

		processMail(ctx, eDB, g, cfg.To, cfg.From, cfg.Subject, rl, cfg.Retries)

		if ctx.Err() != nil {
			queue.RequeueMsgs(ctx, eDB, MailQueueName, failedMsgs[i+1:])

			return ctx.Err()
		}
	}

	// Drain fully; processMail durably queues on cancelled ctx, losing nothing.
	for g := range ch {
		processMail(ctx, eDB, g, cfg.To, cfg.From, cfg.Subject, rl, cfg.Retries)
	}

	return nil
}

// mailInit initializes the mail client once, reusing it across all subsequent calls.
func mailInit(server string, portInt int, username, password string) error {
	mailMu.Lock()
	defer mailMu.Unlock()

	if mailCli == nil {
		logger.Debug().Msg("Initializing e-mail client")

		var err error

		// Mandatory STARTTLS: AUTH PLAIN must never traverse cleartext.
		mailCli, err = mail.NewClient(server,
			mail.WithPort(portInt),
			mail.WithSMTPAuth(mail.SMTPAuthPlain),
			mail.WithTLSPolicy(mail.TLSMandatory),
			mail.WithUsername(username),
			mail.WithPassword(password),
		)
		if err != nil {
			return fmt.Errorf("%w: %w", ErrMailDialer, err)
		}
	}

	return nil
}

// markMailPermanent wraps SMTP errors that cannot succeed on retry in
// retry.Unrecoverable so retry-go short-circuits the remaining attempts.
//
// go-mail's *mail.SendError carries a classifier method IsTemp() that tracks
// the 4xx-vs-5xx split in SMTP reply codes and the underlying error type.
// Treat anything explicitly marked non-temporary (permanent 5xx, auth failure,
// malformed header, broken TLS handshake) as unrecoverable. Any other error
// — network, 4xx, or non-SendError wrapping — keeps its normal retry budget.
func markMailPermanent(err error) error {
	if err == nil {
		return nil
	}

	var sendErr *mail.SendError
	if errors.As(err, &sendErr) && !sendErr.IsTemp() {
		return retry.Unrecoverable(err)
	}

	return err
}

// processMail processes a message and sends it to the specified recipients through the mail service.
//
// It takes the following parameters:
// - ctx: the context.Context object for cancellation and timeouts.
// - eDB: the database instance for checking and storing failed messages.
// - g: the message to be processed and sent.
// - server: the address of the mail server.
// - portInt: the port number for the mail server.
// - username: the username for authentication.
// - password: the password for authentication.
// - to: a slice of email addresses of the recipients.
// - from: the email address of the sender.
// - subject: the subject of the email.
// - rl: the rate limiter to control the message sending rate.
// - retries: the number of retry attempts to send the message.
//
// The function formats the message as a multipart/alternative with both text/plain and text/html
// alternative parts, establishes a dialer, and sends the message to all recipients.
// It logs errors for invalid chat IDs, sending failures, and stores failed messages for retry.
// It uses rate limiting and supports retries with delay.
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

	anyFailed := false
	// Tracks incomplete loops so shutdown-cancelled sends get re-queued, not dropped.
	allProcessed := true

	// Per-recipient sends track partial failures individually.
	for _, u := range to {
		if _, skip := skipSet[u]; skip {
			continue
		}

		m := mail.NewMsg()

		if err := m.From(from); err != nil {
			logger.Error().Msgf("Invalid mail From address %v: %v", from, err)

			anyFailed = true

			continue
		}

		if err := m.To(u); err != nil {
			logger.Error().Msgf("Invalid mail To address %v: %v", u, err)

			anyFailed = true

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

		// Check before rl.Take() so shutdown is not blocked on a token.
		if ctx.Err() != nil {
			allProcessed = false

			break
		}

		rl.Take()

		err := retry.New(
			retry.Attempts(retries),
			retry.Context(ctx),
			retry.Delay(MailMinDelay),
		).Do(
			func() error {
				return markMailPermanent(mailCli.DialAndSendWithContext(ctx, m))
			},
		)
		if err != nil {
			logger.Error().Msgf("%v: %v", ErrMailSendingMessages, err)

			anyFailed = true

			continue
		}

		successfulIDs = append(successfulIDs, u)
	}

	if anyFailed || !allProcessed {
		// Dedup prevents unbounded SkipRecipients growth across retries.
		g.SkipRecipients = mergeSkipRecipients(g.SkipRecipients, successfulIDs)

		// Shutdown-tolerant: queue write must survive ctx cancel.
		sctx, scancel := queueStoreCtx(ctx)
		if err := queue.StoreFailedMsgs(sctx, eDB, MailQueueName, g); err != nil {
			logger.Error().Msgf("%v: %v", queue.ErrQueueing, err)
		}

		scancel()
	}
}
