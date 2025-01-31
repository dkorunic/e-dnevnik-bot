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

	"github.com/avast/retry-go/v4"
	"github.com/dkorunic/e-dnevnik-bot/db"
	"github.com/dkorunic/e-dnevnik-bot/format"
	"github.com/dkorunic/e-dnevnik-bot/logger"
	"github.com/dkorunic/e-dnevnik-bot/msgtypes"
	mail "github.com/wneessen/go-mail"
	"go.uber.org/ratelimit"
)

const (
	MailSendLimit = 20 // 20 emails per 1 hour
	MailWindow    = 1 * time.Hour
	MailMinDelay  = MailWindow / MailSendLimit
	MailSubject   = "Nova ocjena iz e-Dnevnika"
	MailQueue     = "mail-queue"
)

var (
	ErrMailInvalidPort     = errors.New("invalid or missing SMTP port, will try with default 587/tcp")
	ErrMailDialer          = errors.New("failed to create mail delivery client")
	ErrMailSendingMessages = errors.New("error sending mail messages")

	MailQueueName = []byte(MailQueue)
)

// Mail sends a message through the mail service.
//
// The function takes the following parameters:
// - ctx: the context.Context object for cancellation and timeouts.
// - eDB: the database instance for checking failed messages.
// - ch: a channel from which messages are received.
// - server: the address of the mail server.
// - port: the port number for the mail server.
// - username: the username for authentication.
// - password: the password for authentication.
// - from: the email address of the sender.
// - subject: the subject of the email.
// - to: a slice of email addresses of the recipients.
// - retries: the number of retry attempts to send the message.
//
// The function returns an error.
func Mail(ctx context.Context, eDB *db.Edb, ch <-chan interface{}, server, port, username, password, from, subject string, to []string, retries uint) error {
	logger.Debug().Msg("Started e-mail messenger")

	portInt, err := strconv.Atoi(port)
	if err != nil {
		logger.Warn().Msgf("%v: %v", ErrMailInvalidPort, port)

		portInt = 587
	}

	rl := ratelimit.New(MailSendLimit, ratelimit.Per(MailWindow))

	// process all messages
	for o := range ch {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			g, ok := o.(msgtypes.Message)
			if !ok {
				logger.Warn().Msg("Received invalid type from channel, trying to continue")

				continue
			}

			// format message, have both text/plain and text/html alternative
			plainContent := format.PlainMsg(g.Username, g.Subject, g.IsExam, g.Descriptions, g.Fields)
			htmlContent := format.HTMLMsg(g.Username, g.Subject, g.IsExam, g.Descriptions, g.Fields)

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

				break
			}

			var messages []*mail.Msg

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
			err = retry.Do(
				func() error {
					return d.DialAndSend(messages...)
				},
				retry.Attempts(retries),
				retry.Context(ctx),
				retry.Delay(MailMinDelay),
			)
			if err != nil {
				logger.Error().Msgf("%v: %v", ErrMailSendingMessages, err)

				// store failed message
				if err := storeFailedMsgs(eDB, MailQueueName, g); err != nil {
					logger.Error().Msgf("%v: %v", ErrQueueing, err)
				}

				continue
			}
		}
	}

	return nil
}
