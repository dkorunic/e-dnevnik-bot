// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package messenger

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/avast/retry-go/v5"
	"github.com/dkorunic/e-dnevnik-bot/internal/config"
	"github.com/dkorunic/e-dnevnik-bot/internal/format"
	"github.com/dkorunic/e-dnevnik-bot/internal/logger"
	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
	"github.com/dkorunic/e-dnevnik-bot/internal/sqlitedb"
	"go.uber.org/ratelimit"
)

const (
	WebhookAPILimit   = 12 // ntfy.sh's anonymous refill rate, the strictest common target
	WebhookWindow     = 1 * time.Minute
	WebhookMinDelay   = WebhookWindow / WebhookAPILimit
	WebhookQueue      = "webhook-queue"
	WebhookTimeout    = 30 * time.Second
	webhookDrainLimit = 64 << 10 // response bytes read so the connection can be reused

	// Messages in a row answered 5xx before an endpoint is skipped for the cycle.
	// Not one: a 500 can be that message's fault, and it is retried first every
	// cycle, so skipping at once would hold back the queue behind it.
	webhookDownStrikes = 2
)

var (
	ErrWebhookEmptyURLs = errors.New("empty list of webhooks")
	ErrWebhookSending   = errors.New("error sending webhook")
	ErrWebhookStatus    = errors.New("webhook returned an error status")

	// errWebhookDown stands in for an unreachable endpoint's error, which would
	// otherwise stop the whole cycle (see stopsCycle): each URL is its own server.
	errWebhookDown = errors.New("webhook unreachable this cycle")

	// errWebhookServer marks a 5xx, read after retry.Do through lastAttempt.
	errWebhookServer = fmt.Errorf("%w", ErrWebhookStatus)

	WebhookQueueName = []byte(WebhookQueue)

	// No redirects: a 301/302 would turn the POST into a bodyless GET that
	// "succeeds". The endpoint's own status is the answer.
	webhookClient = &http.Client{
		Timeout: WebhookTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
)

// WebhookConfig holds the per-messenger settings for the webhook backend.
type WebhookConfig struct {
	Hooks   []config.Webhook
	Retries uint
}

// webhookPayload is the JSON body. title and message are what Gotify reads;
// the rest is for automations (Home Assistant, n8n) that branch on the event.
type webhookPayload struct {
	Title        string   `json:"title"`
	Message      string   `json:"message"`
	Event        string   `json:"event"`
	Username     string   `json:"username"`
	Subject      string   `json:"subject"`
	Descriptions []string `json:"descriptions"`
	Fields       []string `json:"fields"`
}

// Webhook resends queued failures, then POSTs ch to every configured URL.
// The URL is the recipient ID, and is never logged whole (see config.WebhookLabel).
func Webhook(ctx context.Context, eDB *sqlitedb.Edb, ch <-chan msgtypes.Message, cfg WebhookConfig) (err error) {
	// Panic guard; stays nil on the resend path (see recoverMessenger).
	var inflight *msgtypes.Message

	defer func() {
		if r := recover(); r != nil {
			err = recoverMessenger(ctx, eDB, WebhookQueueName, ch, r, inflight)
		}
	}()

	if len(cfg.Hooks) == 0 {
		queueUndelivered(ctx, eDB, WebhookQueueName, ch)

		return fmt.Errorf("%w", ErrWebhookEmptyURLs)
	}

	logger.Debug().Msg("Started webhook messenger")

	rl := ratelimit.New(WebhookAPILimit, ratelimit.Per(WebhookWindow))

	// Strikes per endpoint this cycle. At webhookDownStrikes it is skipped (and
	// requeued) for the rest of it rather than costing every message the full
	// retry delay.
	down := make(map[string]int, len(cfg.Hooks))

	var cs cycleSender

	resendQueued(ctx, eDB, WebhookQueueName, func(m msgtypes.Message) {
		cs.send(ctx, eDB, WebhookQueueName, m, func() bool {
			return processWebhook(ctx, eDB, m, cfg, rl, down)
		})
	})

	// Drain fully; processWebhook durably queues on cancelled ctx, losing nothing.
	for g := range ch {
		inflight = &g
		cs.send(ctx, eDB, WebhookQueueName, g, func() bool {
			return processWebhook(ctx, eDB, g, cfg, rl, down)
		})
		inflight = nil
	}

	return sendFailureErr(eDB, WebhookQueueName)
}

func processWebhook(ctx context.Context, eDB *sqlitedb.Edb, g msgtypes.Message, cfg WebhookConfig, rl ratelimit.Limiter,
	down map[string]int,
) bool {
	run := newRecipientRun(g)

	for _, h := range cfg.Hooks {
		if run.skipped(h.URL) {
			continue
		}

		if down[h.URL] >= webhookDownStrikes {
			run.failed(errWebhookDown)

			continue
		}

		// Shutdown must not wait out a token: interrupt and requeue.
		if !takeCtx(ctx, rl) {
			run.interrupt()

			break
		}

		err := retry.New(
			retry.Attempts(cfg.Retries),
			retry.Context(ctx),
			retry.Delay(WebhookMinDelay),
		).Do(
			func() error {
				return postWebhook(ctx, h, g)
			},
		)
		if err != nil {
			logger.Error().Msgf("%v %v: %v", ErrWebhookSending, config.WebhookLabel(h.URL), err)

			// Never a config error either: each endpoint has its own
			// credentials, so nothing here condemns the others.
			switch {
			case isUnreachable(err):
				down[h.URL] = webhookDownStrikes
				err = errWebhookDown
			case errors.Is(lastAttempt(err), errWebhookServer):
				down[h.URL]++
			}

			run.failed(err)

			continue
		}

		delete(down, h.URL)
		run.delivered(h.URL)
	}

	return run.finish(ctx, eDB, WebhookQueueName, g)
}

// postWebhook sends one attempt, classified for retry.Do:
//   - network errors, 5xx, 408, 429: transient, retried;
//   - anything else non-2xx (3xx, other 4xx): retrying is pointless and the
//     cause unknown — a revoked token and a rejected body look alike — so that
//     endpoint alone is requeued (bare Unrecoverable). Never poison-dropped: a
//     401 is a credential problem the user can fix.
func postWebhook(ctx context.Context, h config.Webhook, g msgtypes.Message) error {
	body, contentType, err := webhookBody(h.Format, g)
	if err != nil {
		return retry.Unrecoverable(err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.URL, bytes.NewReader(body))
	if err != nil {
		return retry.Unrecoverable(err)
	}

	req.Header.Set("Content-Type", contentType)

	for k, v := range h.Headers {
		req.Header.Set(k, v)
	}

	resp, err := webhookClient.Do(req)
	if err != nil {
		// url.Error quotes the whole URL, secret path included; its cause still
		// carries the net.Error that isUnreachable reads.
		if ue, ok := errors.AsType[*url.Error](err); ok {
			return ue.Err
		}

		return err
	}

	defer resp.Body.Close()

	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, webhookDrainLimit))

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}

	status := ErrWebhookStatus
	if resp.StatusCode >= http.StatusInternalServerError {
		status = errWebhookServer
	}

	err = fmt.Errorf("%w: HTTP %d", status, resp.StatusCode)

	if resp.StatusCode < 400 || isPermanentHTTPStatus(resp.StatusCode) {
		return retry.Unrecoverable(err)
	}

	return err
}

// webhookBody renders g as the endpoint's format: the JSON payload, or the
// plain-text alert (ntfy publishes a text body as is).
func webhookBody(f string, g msgtypes.Message) ([]byte, string, error) {
	if f == config.WebhookText {
		return []byte(format.PlainMsg(g.Username, g.Subject, g.Code, g.Descriptions, g.Fields)), "text/plain; charset=utf-8", nil
	}

	b, err := json.Marshal(webhookPayload{
		Title:        format.PlainSubject(g.Username, g.Subject, g.Code),
		Message:      format.PlainBody(g.Descriptions, g.Fields),
		Event:        eventName(g.Code),
		Username:     g.Username,
		Subject:      g.Subject,
		Descriptions: g.Descriptions,
		Fields:       g.Fields,
	})

	return b, "application/json", err
}

// eventName is code's config-facing name (msgtypes.EventCodeNames).
func eventName(code msgtypes.EventCode) string {
	for name, c := range msgtypes.EventCodeNames {
		if c == code {
			return name
		}
	}

	return ""
}
