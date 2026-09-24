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
	CalDAVAPILimit = 30 // no published quota; polite to self-hosted servers
	CalDAVWindow   = 1 * time.Minute
	CalDAVMinDelay = CalDAVWindow / CalDAVAPILimit
	CalDAVTimeout  = 30 * time.Second
	CalDAVQueue    = "caldav-queue"

	// caldavMaxDrain bounds the discarded body read that lets the connection
	// be reused.
	caldavMaxDrain = 64 << 10
)

var (
	CalDAVQueueName = []byte(CalDAVQueue)

	// Shared for connection reuse. Credentials travel per request, so no
	// credGuard.
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

// newCalDAVClient owns its transport so nothing else in the process can
// reconfigure it.
//
// Redirects are refused: net/http replays a redirected PUT as a bodiless GET,
// whose 200 would pass for a stored exam, and keeps Authorization across a
// same-host https→http hop.
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
// collection. Nothing is initialised up front — the first PUT is the credential
// check — so, unlike Calendar, there is no drain-on-init-failure path.
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

// markCalDAVPermanent stops retry-go on an answer that cannot change: a
// redirect (the configured URL is wrong) or a 4xx. 423 Locked stays transient
// alongside 408, 429 and 5xx — another client's lock is released, and dropping
// would lose the exam. 412 never gets here: putCalDAV reads it as success.
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
		logger.Error().Msgf("Permanently dropping CalDAV event for %v/%v (will not retry): %v",
			g.Username, g.Subject, err)

		return
	}

	logger.Error().Msgf("Unable to store CalDAV event: %v", err)

	storeCalDAV(ctx, eDB, g)
}

// putCalDAV sends one create-only PUT and classifies the answer for retry-go.
func putCalDAV(ctx context.Context, cfg CalDAVConfig, target string, body []byte) error {
	// bytes.Reader gives the request a GetBody, so a replay can rewind.
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

	// Already stored. Decided here, not after retry.Do: its error aggregates
	// every attempt, so a post-loop check would see an earlier transient error
	// first and report a stored event as dropped.
	if resp.StatusCode == http.StatusPreconditionFailed {
		logger.Debug().Msgf("CalDAV event already exists (idempotent insert): %v", path.Base(target))

		return nil
	}

	se := &caldavStatusError{code: resp.StatusCode}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		// Server-supplied, so it may carry userinfo.
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
