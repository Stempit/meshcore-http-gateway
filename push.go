package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/stempit/meshcore-kit/companion"
)

// respDrainMax caps how many bytes of a push response body are read before
// discarding the rest — responses carry nothing we need, but the body must
// be drained for the connection to be reusable.
const respDrainMax = 4 << 10

// pusher delivers each new inbox message to a configured HTTP endpoint as a
// single POST. It polls the inbox with its own cursor, so push and pull
// clients coexist: messages are copied into a private pending queue when
// read, and a GET /get drain cannot take them away. Delivery is strictly
// sequential by ascending ID.
//
// Tuning: interval is the worst-case push latency (250ms poll of the inbox);
// while the endpoint is down, deliveries queue up to pendingCap entries
// (oldest dropped, like the inbox) and failing attempts are retried with
// exponential backoff between backoffStart and backoffMax, evaluated once
// per poll tick.
type pusher struct {
	endpoint string
	token    string
	timeout  time.Duration
	inbox    *companion.Inbox
	client   *http.Client
	log      *slog.Logger

	// Tunables, overridable in tests.
	interval     time.Duration
	backoffStart time.Duration
	backoffMax   time.Duration
	pendingCap   int

	// started, when non-nil, is closed once run has taken its startup
	// cursor. Tests use it to add "live" messages only after the worker
	// is actually running.
	started chan struct{}
}

func newPusher(cfg *config, inbox *companion.Inbox, log *slog.Logger) *pusher {
	return &pusher{
		endpoint:     cfg.Push.EndpointURL,
		token:        cfg.Push.Token,
		timeout:      time.Duration(cfg.Push.TimeoutSeconds) * time.Second,
		inbox:        inbox,
		client:       &http.Client{},
		log:          log,
		interval:     250 * time.Millisecond,
		backoffStart: 2 * time.Second,
		backoffMax:   60 * time.Second,
		pendingCap:   1000,
	}
}

// run drives the worker until ctx is done. Live-only: the cursor starts at
// the inbox's current last ID, so messages already present at startup (the
// stored-message sync) are not pushed — pull mode is the replay path. The
// pending queue lives in memory only and is lost on shutdown.
func (p *pusher) run(ctx context.Context) {
	cursor := p.inbox.LastID()
	if p.started != nil {
		close(p.started)
	}
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()

	var (
		pending []companion.Message
		wait    time.Duration // current backoff; 0 = endpoint healthy
		nextTry time.Time     // earliest next attempt, meaningful while wait > 0
	)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		// Take ownership of everything new and advance the cursor in the
		// same breath: the pending queue is the only copy push mode keeps.
		// This runs every tick, including while backing off, so an outage
		// queues messages here (bounded, drop-oldest) instead of leaving
		// them exposed to pull clients.
		msgs, last := p.inbox.SinceWithLastID(cursor)
		cursor = last
		pending = p.enqueue(pending, msgs)

		if wait > 0 && time.Now().Before(nextTry) {
			continue
		}
		if len(pending) == 0 {
			continue
		}

		outcome, detail := p.post(ctx, pending[0])
		if ctx.Err() != nil {
			return
		}
		switch outcome {
		case delivered:
			p.log.Info("push delivered", "id", pending[0].ID, "type", pending[0].Type,
				"sender", pending[0].Sender, "result", detail)
			pending = pending[1:]
			wait = 0
		case rejected:
			// The endpoint answered and does not want this message —
			// retrying would only repeat the rejection.
			p.log.Warn("push endpoint rejected message, dropping", "id", pending[0].ID, "result", detail)
			pending = pending[1:]
			wait = 0
		case transient:
			if wait == 0 {
				wait = p.backoffStart
			} else if wait < p.backoffMax {
				wait = min(wait*2, p.backoffMax)
			}
			nextTry = time.Now().Add(wait)
			p.log.Warn("push delivery failed, will retry", "id", pending[0].ID,
				"result", detail, "retry_in", wait)
		}
	}
}

// enqueue appends msgs to the pending queue, dropping the OLDEST entries
// when the cap is exceeded — the same overflow policy as the inbox.
func (p *pusher) enqueue(pending []companion.Message, msgs []companion.Message) []companion.Message {
	pending = append(pending, msgs...)
	if overflow := len(pending) - p.pendingCap; overflow > 0 {
		p.log.Warn("push queue overflow, dropping oldest", "dropped", overflow)
		pending = append(pending[:0], pending[overflow:]...)
	}
	return pending
}

// outcome classifies one delivery attempt.
type outcome int

const (
	delivered outcome = iota // any 2xx: the endpoint has the message
	rejected                 // any other 4xx: it does not want it; retrying won't help
	transient                // network error, timeout, 429 or 5xx: retry later
)

// post sends one message and classifies the result. detail is a human-readable
// summary for logs ("status 200", "context deadline exceeded"). Shutdown
// (ctx done) is not classified — the run loop checks ctx itself.
func (p *pusher) post(ctx context.Context, msg companion.Message) (outcome, string) {
	body, err := json.Marshal(msg)
	if err != nil {
		// companion.Message is plain JSON data; this cannot happen.
		return rejected, fmt.Sprintf("marshal: %v", err)
	}

	dctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(dctx, http.MethodPost, p.endpoint, bytes.NewReader(body))
	if err != nil {
		return transient, err.Error()
	}
	req.Header.Set("Content-Type", "application/json")
	if p.token != "" {
		req.Header.Set("Authorization", "Bearer "+p.token)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return transient, err.Error()
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, respDrainMax))

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return delivered, fmt.Sprintf("status %d", resp.StatusCode)
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return transient, fmt.Sprintf("status %d", resp.StatusCode)
	default:
		return rejected, fmt.Sprintf("status %d", resp.StatusCode)
	}
}
