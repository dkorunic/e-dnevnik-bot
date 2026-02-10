// @license
// Copyright (C) 2022  Dinko Korunic
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
	"errors"
	"strconv"
	"time"

	"github.com/avast/retry-go/v5"
	"github.com/dkorunic/e-dnevnik-bot/format"
	"github.com/dkorunic/e-dnevnik-bot/logger"
	"github.com/dkorunic/e-dnevnik-bot/msgtypes"
	"github.com/dkorunic/e-dnevnik-bot/queue"
	"github.com/dkorunic/e-dnevnik-bot/sqlitedb"
	"github.com/dkorunic/e-dnevnik-bot/version"
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
)

// Mail sends messages through the mail service to the specified recipients.
//
// It takes the following parameters:
// - ctx: the context.Context object for cancellation and timeouts.
// - eDB: the database instance for checking and storing failed messages.
// - ch: the channel from which to receive messages.
// - server: the address of the mail server.
// - port: the port number for the mail server as a string.
// - username: the username for authentication.
// - password: the password for authentication.
// - from: the email address of the sender.
// - subject: the subject of the email.
// - to: a slice of email addresses of the recipients.
// - retries: the number of retry attempts for sending the message.
//
// The function processes all failed messages and new messages, sending them
// through the mail service. It uses a rate limiter to control the message
// sending rate and supports retry attempts for sending failures. It logs
// invalid ports and sets a default port if necessary.
func Mail(ctx context.Context, eDB *sqlitedb.Edb, ch <-chan msgtypes.Message, server, port, username, password, from, subject string, to []string, retries uint) error {
	logger.Debug().Msgf("Started e-mail messenger (%v)", MailVersion)

	portInt, err := strconv.Atoi(port)
	if err != nil {
		logger.Warn().Msgf("%v: %v", ErrMailInvalidPort, port)

		portInt = 587
	}

	rl := ratelimit.New(MailSendLimit, ratelimit.Per(MailWindow))

	var g msgtypes.Message

	// process all failed messages
	for _, g = range queue.FetchFailedMsgs(ctx, eDB, MailQueueName) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			processMail(ctx, eDB, g, server, portInt, username, password, to, from, subject, rl, retries)
		}
	}

	// process all messages
	for g = range ch {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			processMail(ctx, eDB, g, server, portInt, username, password, to, from, subject, rl, retries)
		}
	}

	return nil
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
func processMail(ctx context.Context, eDB *sqlitedb.Edb, g msgtypes.Message, server string, portInt int, username string,
	password string, to []string, from, subject string, rl ratelimit.Limiter, retries uint,
) {
	// format message, have both text/plain and text/html alternative
	plainContent := format.PlainMsg(g.Username, g.Subject, g.Code, g.Descriptions, g.Fields)
	htmlContent := format.HTMLMsg(g.Username, g.Subject, g.Code, g.Descriptions, g.Fields)

	// establish dialer
	d, err := mail.NewClient(server,
		mail.WithPort(portInt),
		mail.WithSMTPAuth(mail.SMTPAuthPlain),
		mail.WithTLSPolicy(mail.TLSOpportunistic),
		mail.WithUsername(username),
		mail.WithPassword(password),
	)
	if err != nil {
		logger.Error().Msgf("%v: %v", ErrMailDialer, err)

		return
	}

	messages := make([]*mail.Msg, 0, len(to))

	// bulk send to all recipients
	for _, u := range to {
		m := mail.NewMsg()

		_ = m.From(from)
		_ = m.To(u)

		m.SetMessageID()
		m.SetDate()
		m.SetBulk()

		if subject != "" {
			m.Subject(subject)
		} else {
			m.Subject(MailSubject)
		}

		m.SetBodyString(mail.TypeTextPlain, plainContent)
		m.AddAlternativeString(mail.TypeTextHTML, htmlContent)

		messages = append(messages, m)
	}

	rl.Take()

	// retryable and cancellable attempt to send a message
	err = retry.New(
		retry.Attempts(retries),
		retry.Context(ctx),
		retry.Delay(MailMinDelay),
	).Do(
		func() error {
			return d.DialAndSend(messages...)
		},
	)
	if err != nil {
		logger.Error().Msgf("%v: %v", ErrMailSendingMessages, err)

		// store failed message
		if err := queue.StoreFailedMsgs(ctx, eDB, MailQueueName, g); err != nil {
			logger.Error().Msgf("%v: %v", queue.ErrQueueing, err)
		}

		return
	}
}
