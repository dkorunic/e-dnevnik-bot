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
	"github.com/dkorunic/e-dnevnik-bot/format"
	"github.com/dkorunic/e-dnevnik-bot/logger"
	"github.com/dkorunic/e-dnevnik-bot/msgtypes"
	"gopkg.in/mail.v2"
)

const (
	MailSendDelay = 50 * time.Millisecond
	MailSubject   = "Nova ocjena iz e-Dnevnika"
)

var (
	ErrMailSendingMessage = errors.New("error sending mail message")
	ErrMailInvalidPort    = errors.New("invalid or missing SMTP port, will try with default 465/tcp")
)

// Mail messenger processes events from a channel and attempts to send emails to one or more recipients,
// optionally returning an error.
func Mail(ctx context.Context, ch <-chan interface{}, server, port, username, password, from, subject string, to []string, retries uint) error {
	logger.Debug().Msg("Sending message through mail service")

	portInt, err := strconv.Atoi(port)
	if err != nil {
		logger.Warn().Msgf("%v: %v", ErrMailInvalidPort, port)

		portInt = 465
	}

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
			d := mail.NewDialer(server, portInt, username, password)
			d.StartTLSPolicy = mail.MandatoryStartTLS

			// send to all recipients
			for _, u := range to {
				m := mail.NewMessage()
				m.SetHeader("From", from)
				m.SetHeader("To", u)

				if subject != "" {
					m.SetHeader("Subject", subject)
				} else {
					m.SetHeader("Subject", MailSubject)
				}

				m.SetBody("text/plain", plainContent)
				m.AddAlternative("text/html", htmlContent)

				// retryable and cancellable attempt to send a message
				err = retry.Do(
					func() error {
						return d.DialAndSend(m)
					},
					retry.Attempts(retries),
					retry.Context(ctx),
				)
				if err != nil {
					logger.Error().Msgf("%v: %v", ErrMailSendingMessage, err)

					break
				}

				time.Sleep(MailSendDelay)
			}
		}
	}

	return err
}
