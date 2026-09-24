// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package messenger

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"time"

	"github.com/avast/retry-go/v5"
	"github.com/dkorunic/e-dnevnik-bot/internal/logger"
	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
	"github.com/dkorunic/e-dnevnik-bot/internal/queue"
	"github.com/dkorunic/e-dnevnik-bot/internal/sqlitedb"
	"go.uber.org/ratelimit"
)

const (
	CalDAVAPILimit = 30 // self-hosted servers publish no quota; keeps a burst polite
	CalDAVWindow   = 1 * time.Minute
	CalDAVMinDelay = CalDAVWindow / CalDAVAPILimit
	CalDAVTimeout  = 30 * time.Second
	CalDAVQueue    = "caldav-queue"

	// caldavMaxDrain bounds the response body read only to recycle the
	// connection.
	caldavMaxDrain = 64 << 10
)

var (
	CalDAVQueueName = []byte(CalDAVQueue)

	// Shared for connection reuse. It carries no credentials — they go on each
	// request — so unlike the SDK-backed messengers it needs no credGuard.
	caldavClient = newCalDAVClient()
)

// CalDAVConfig holds the per-messenger settings for the CalDAV backend.
type CalDAVConfig struct {
	URL      string
	Username string
	Password string
	Retries  uint
}

// caldavStatusError is a non-2xx answer to the PUT.
type caldavStatusError struct {
	location string
	code     int
}

func (e *caldavStatusError) Error() string {
	if e.location != "" {
		return fmt.Sprintf("CalDAV server answered %d %s redirecting to %q; configure that collection URL instead",
			e.code, http.StatusText(e.code), e.location)
	}

	return fmt.Sprintf("CalDAV server answered %d %s", e.code, http.StatusText(e.code))
}

// newCalDAVClient builds a client on its own transport, never
// http.DefaultTransport, so nothing else in the process can reconfigure it.
//
// Redirects are refused. net/http replays a 301/302/303 PUT as a bodiless GET,
// whose 200 would read as a stored exam while nothing was written; and it keeps
// Authorization across a same-host https→http hop, sending the password in
// cleartext.
func newCalDAVClient() *http.Client {
	return &http.Client{
		Timeout: CalDAVTimeout,
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          2,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// CalDAV resends queued failures, then PUTs ch's exams into the configured
// collection. There is no fallible init — credentials are checked by the first
// PUT — so, unlike Calendar, there is no drain-on-init-failure branch.
func CalDAV(ctx context.Context, eDB *sqlitedb.Edb, ch <-chan msgtypes.Message, cfg CalDAVConfig) (err error) {
	// Panic guard; stays nil on the resend path (see recoverMessenger).
	var inflight *msgtypes.Message

	defer func() {
		if r := recover(); r != nil {
			err = recoverMessenger(ctx, eDB, CalDAVQueueName, ch, r, inflight)
		}
	}()

	logger.Debug().Msg("Started CalDAV messenger")

	rl := ratelimit.New(CalDAVAPILimit, ratelimit.Per(CalDAVWindow))

	resendQueued(ctx, eDB, CalDAVQueueName, func(m msgtypes.Message) {
		processCalDAV(ctx, eDB, m, rl, cfg)
	})

	// Drain fully; processCalDAV durably queues on cancelled ctx, losing nothing.
	for g := range ch {
		inflight = &g
		processCalDAV(ctx, eDB, g, rl, cfg)
		inflight = nil
	}

	return nil
}

// markCalDAVPermanent stops retry-go on an answer that will never change: a
// redirect (the configured URL is wrong) or a permanent 4xx. putCalDAV
// intercepts 412 before it ever reaches here — If-None-Match: * means the
// event is already stored, so it is success, not a permanent failure — but
// this still classifies a 412 as permanent if ever handed one directly (see
// TestMarkCalDAVPermanentClasses). 408, 423, 429, 5xx and transport errors stay
// transient: WebDAV's 423 Locked means another client holds a lock, which is
// released, so dropping on it would lose the exam for good.
func markCalDAVPermanent(err error) error {
	if err == nil {
		return nil
	}

	if se, ok := errors.AsType[*caldavStatusError](err); ok {
		isRedirect := se.code >= 300 && se.code < 400
		isLocked := se.code == http.StatusLocked
		if isRedirect || (isPermanentHTTPStatus(se.code) && !isLocked) {
			// Inner sentinel survives retry.Do's marker stripping.
			return retry.Unrecoverable(permanentError{err})
		}
	}

	return err
}

// processCalDAV PUTs g as a create-only all-day event, re-queueing on a
// transient failure. examEventOf decides what is sent; its ID names the
// resource, so a repeat answers 412 instead of duplicating.
func processCalDAV(ctx context.Context, eDB *sqlitedb.Edb, g msgtypes.Message, rl ratelimit.Limiter, cfg CalDAVConfig) {
	ev, ok := examEventOf("CalDAV", g)
	if !ok {
		return
	}

	// Cancelled before the PUT: re-queue rather than be dropped.
	if ctx.Err() != nil {
		storeCalDAV(ctx, eDB, g)

		return
	}

	target, err := url.JoinPath(cfg.URL, ev.ID+".ics")
	if err != nil {
		// Unreachable for a URL that passed config validation.
		logger.Error().Msgf("Permanently dropping CalDAV event for %v/%v: cannot build resource URL: %v",
			g.Username, g.Subject, err)

		return
	}

	body := buildICalEvent(ev, time.Now())

	rl.Take()

	err = retry.New(
		retry.Attempts(cfg.Retries),
		retry.Context(ctx),
		retry.Delay(CalDAVMinDelay),
	).Do(
		func() error {
			return putCalDAV(ctx, cfg, target, body)
		},
	)
	if err == nil {
		return
	}

	if isPermanentSendErr(err) {
		// putCalDAV already turned a 412 into a nil-error success above, so
		// anything permanent reaching here is a real drop, don't requeue.
		logger.Error().Msgf("Permanently dropping CalDAV event for %v/%v (will not retry): %v",
			g.Username, g.Subject, err)

		return
	}

	logger.Error().Msgf("Unable to store CalDAV event: %v", err)

	storeCalDAV(ctx, eDB, g)
}

// putCalDAV sends one create-only PUT and classifies the answer.
func putCalDAV(ctx context.Context, cfg CalDAVConfig, target string, body []byte) error {
	// A bytes.Reader body makes NewRequest set GetBody, so a replayed request
	// can rewind.
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, target, bytes.NewReader(body))
	if err != nil {
		return retry.Unrecoverable(permanentError{err})
	}

	req.SetBasicAuth(cfg.Username, cfg.Password)
	req.Header.Set("Content-Type", "text/calendar; charset=utf-8")
	// Create-only: an existing resource answers 412 rather than being replaced.
	req.Header.Set("If-None-Match", "*")

	resp, err := caldavClient.Do(req)
	if err != nil {
		return err
	}

	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, caldavMaxDrain))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}

	// If-None-Match: * means the event is already stored — treat it as success
	// here, at the source, rather than through markCalDAVPermanent: retry-go's
	// returned error carries every attempt, and a post-loop check keyed on it
	// would find an earlier transient attempt's error first, misreporting a
	// stored event as a permanent failure.
	if resp.StatusCode == http.StatusPreconditionFailed {
		logger.Debug().Msgf("CalDAV event already exists (idempotent insert): %v", path.Base(target))

		return nil
	}

	se := &caldavStatusError{code: resp.StatusCode}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		loc := resp.Header.Get("Location")
		if u, err := url.Parse(loc); err == nil {
			loc = u.Redacted()
		}

		se.location = loc
	}

	return markCalDAVPermanent(se)
}

// storeCalDAV queues g for the next cycle; the write must survive ctx cancel.
func storeCalDAV(ctx context.Context, eDB *sqlitedb.Edb, g msgtypes.Message) {
	sctx, scancel := queueStoreCtx(ctx)
	defer scancel()

	if err := queue.StoreFailedMsgs(sctx, eDB, CalDAVQueueName, g); err != nil {
		logger.Error().Msgf("%v: %v", queue.ErrQueueing, err)
	}
}
