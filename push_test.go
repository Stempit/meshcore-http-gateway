package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stempit/meshcore-kit/companion"
)

// recordedPush is one POST the push target received.
type recordedPush struct {
	msg         companion.Message
	status      int    // status code the target answered with
	auth        string // Authorization header
	contentType string // Content-Type header
}

// pushTarget is a webhook endpoint for tests: every push POST is recorded on
// the pushes channel and answered with the next status code sent on codes
// (the last one stays in effect when codes is empty).
type pushTarget struct {
	srv    *httptest.Server
	URL    string
	pushes <-chan recordedPush
	codes  chan<- int
}

func newPushTarget(t *testing.T, codes ...int) *pushTarget {
	t.Helper()
	pushes := make(chan recordedPush, 64)
	codeCh := make(chan int, len(codes)+16) // fits every queued code without blocking
	for _, c := range codes {
		codeCh <- c
	}
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case status = <-codeCh:
		default:
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading push body: %v", err)
		}
		var msg companion.Message
		if err := json.Unmarshal(body, &msg); err != nil {
			t.Errorf("push body is not a companion.Message: %v (%q)", err, body)
		}
		pushes <- recordedPush{
			msg:         msg,
			status:      status,
			auth:        r.Header.Get("Authorization"),
			contentType: r.Header.Get("Content-Type"),
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return &pushTarget{srv: srv, URL: srv.URL, pushes: pushes, codes: codeCh}
}

func newTestPusher(endpoint string, inbox *companion.Inbox) *pusher {
	return &pusher{
		endpoint:     endpoint,
		timeout:      time.Second,
		inbox:        inbox,
		client:       &http.Client{},
		log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		interval:     5 * time.Millisecond,
		backoffStart: time.Millisecond,
		backoffMax:   5 * time.Millisecond,
		pendingCap:   1000,
	}
}

func startPusher(t *testing.T, p *pusher) {
	t.Helper()
	p.started = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go p.run(ctx)
	select {
	case <-p.started:
	case <-time.After(time.Second):
		t.Fatal("pusher did not start")
	}
}

func waitForPush(t *testing.T, pushes <-chan recordedPush) recordedPush {
	t.Helper()
	select {
	case p := <-pushes:
		return p
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a push")
		return recordedPush{}
	}
}

func assertNoPush(t *testing.T, pushes <-chan recordedPush) {
	t.Helper()
	select {
	case p := <-pushes:
		t.Fatalf("unexpected push: %+v", p.msg)
	case <-time.After(60 * time.Millisecond):
	}
}

func TestPushDeliversDMAndChannel(t *testing.T) {
	target := newPushTarget(t, http.StatusOK)
	inbox := &companion.Inbox{}
	startPusher(t, newTestPusher(target.URL, inbox))

	dm := inbox.Add(companion.Message{Type: "dm", Sender: "Alice", SenderKey: "a1b2", ChannelIndex: -1, Text: "ping"})
	ch := inbox.Add(companion.Message{Type: "channel", Sender: "Bob", Channel: "public", ChannelIndex: 0, Text: "hello"})

	got := waitForPush(t, target.pushes)
	if got.msg.ID != dm.ID || got.msg.Type != "dm" || got.msg.Sender != "Alice" ||
		got.msg.SenderKey != "a1b2" || got.msg.ChannelIndex != -1 || got.msg.Channel != "" {
		t.Errorf("first push = %+v, want dm %d", got.msg, dm.ID)
	}
	if got.contentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got.contentType)
	}
	if got.auth != "" {
		t.Errorf("Authorization = %q, want none without push.token", got.auth)
	}

	got = waitForPush(t, target.pushes)
	if got.msg.ID != ch.ID || got.msg.Type != "channel" || got.msg.Sender != "Bob" ||
		got.msg.Channel != "public" || got.msg.ChannelIndex != 0 || got.msg.SenderKey != "" {
		t.Errorf("second push = %+v, want channel %d", got.msg, ch.ID)
	}
}

func TestPushSendsBearerToken(t *testing.T) {
	target := newPushTarget(t, http.StatusOK)
	inbox := &companion.Inbox{}
	p := newTestPusher(target.URL, inbox)
	p.token = "s3cret"
	startPusher(t, p)

	inbox.Add(companion.Message{Type: "dm", Sender: "Alice", ChannelIndex: -1, Text: "hi"})
	got := waitForPush(t, target.pushes)
	if got.auth != "Bearer s3cret" {
		t.Errorf("Authorization = %q, want Bearer s3cret", got.auth)
	}
}

func TestPushCursorStartsAtLastID(t *testing.T) {
	target := newPushTarget(t, http.StatusOK)
	inbox := &companion.Inbox{}
	inbox.Add(companion.Message{Type: "dm", Text: "already here"})

	startPusher(t, newTestPusher(target.URL, inbox))
	assertNoPush(t, target.pushes) // stored-sync messages must not be pushed

	added := inbox.Add(companion.Message{Type: "dm", Text: "live"})
	got := waitForPush(t, target.pushes)
	if got.msg.ID != added.ID || got.msg.Text != "live" {
		t.Errorf("push = %+v, want only the live message %d", got.msg, added.ID)
	}
}

func TestPushRetriesTransientAndKeepsOrder(t *testing.T) {
	target := newPushTarget(t, http.StatusInternalServerError, http.StatusOK, http.StatusOK)
	inbox := &companion.Inbox{}
	startPusher(t, newTestPusher(target.URL, inbox))

	first := inbox.Add(companion.Message{Type: "dm", Text: "one"})
	second := inbox.Add(companion.Message{Type: "channel", Text: "two"})

	got := waitForPush(t, target.pushes)
	if got.msg.ID != first.ID || got.status != http.StatusInternalServerError {
		t.Fatalf("first attempt = %+v (status %d), want %d with 500", got.msg, got.status, first.ID)
	}
	got = waitForPush(t, target.pushes)
	if got.msg.ID != first.ID || got.status != http.StatusOK {
		t.Fatalf("retry = %+v (status %d), want %d with 200 (transient failure must redeliver)", got.msg, got.status, first.ID)
	}
	got = waitForPush(t, target.pushes)
	if got.msg.ID != second.ID || got.status != http.StatusOK {
		t.Errorf("next = %+v (status %d), want %d with 200 (order must be preserved)", got.msg, got.status, second.ID)
	}
}

func TestPushDropsRejected(t *testing.T) {
	target := newPushTarget(t, http.StatusForbidden, http.StatusOK)
	inbox := &companion.Inbox{}
	startPusher(t, newTestPusher(target.URL, inbox))

	first := inbox.Add(companion.Message{Type: "dm", Text: "nope"})
	second := inbox.Add(companion.Message{Type: "dm", Text: "yes"})

	got := waitForPush(t, target.pushes)
	if got.msg.ID != first.ID || got.status != http.StatusForbidden {
		t.Fatalf("first attempt = %+v (status %d), want %d with 403", got.msg, got.status, first.ID)
	}
	got = waitForPush(t, target.pushes)
	if got.msg.ID != second.ID || got.msg.Text != "yes" {
		t.Errorf("next = %+v, want %d (rejected message must be dropped, not redelivered)", got.msg, second.ID)
	}
}

func TestPushQueueSurvivesPullDrain(t *testing.T) {
	// Two failures keep the endpoint down long enough for the drain to
	// happen between attempts; delivery only lands on the third attempt.
	target := newPushTarget(t,
		http.StatusInternalServerError, http.StatusInternalServerError, http.StatusOK)
	inbox := &companion.Inbox{}
	startPusher(t, newTestPusher(target.URL, inbox))

	added := inbox.Add(companion.Message{Type: "dm", Text: "keep me"})
	got := waitForPush(t, target.pushes) // failed attempt: message now owned by the pusher
	if got.msg.ID != added.ID || got.status != http.StatusInternalServerError {
		t.Fatalf("attempt = %+v (status %d), want %d with 500", got.msg, got.status, added.ID)
	}

	// A pull client drains the inbox while the endpoint is still down.
	inbox.Drain()

	got = waitForPush(t, target.pushes)
	if got.msg.ID != added.ID || got.status != http.StatusInternalServerError {
		t.Fatalf("retry = %+v (status %d), want %d with 500", got.msg, got.status, added.ID)
	}
	got = waitForPush(t, target.pushes)
	if got.msg.ID != added.ID || got.msg.Text != "keep me" || got.status != http.StatusOK {
		t.Errorf("recovery push = %+v (status %d), want %d with 200 (a /get drain must not eat the push queue)",
			got.msg, got.status, added.ID)
	}
}

func TestPushQueueOverflowDropsOldest(t *testing.T) {
	// Four live messages arrive while the endpoint fails; the cap-2 pending
	// queue keeps only the two newest. Recovery is gated on the first 2xx,
	// long after every add has been ingested, so a and b are gone by then.
	target := newPushTarget(t,
		http.StatusInternalServerError, http.StatusInternalServerError,
		http.StatusInternalServerError, http.StatusInternalServerError,
		http.StatusInternalServerError, http.StatusInternalServerError,
		http.StatusInternalServerError, http.StatusInternalServerError,
		http.StatusInternalServerError, http.StatusInternalServerError,
		http.StatusInternalServerError, http.StatusInternalServerError,
		http.StatusInternalServerError, http.StatusInternalServerError,
		http.StatusInternalServerError, http.StatusInternalServerError,
		http.StatusInternalServerError, http.StatusInternalServerError,
		http.StatusInternalServerError, http.StatusInternalServerError,
		http.StatusOK, http.StatusOK)
	inbox := &companion.Inbox{}
	p := newTestPusher(target.URL, inbox)
	p.pendingCap = 2
	startPusher(t, p)

	for _, text := range []string{"a", "b", "c", "d"} {
		inbox.Add(companion.Message{Type: "dm", Text: text})
	}

	// Skip the failures (bounded, so a never-recovers regression fails the
	// test instead of looping); the first 2xx must be the oldest survivor.
	var got recordedPush
	for recovered := false; !recovered; {
		got = waitForPush(t, target.pushes)
		recovered = got.status == http.StatusOK
	}
	if got.msg.Text != "c" {
		t.Errorf("first delivery after recovery = %q, want \"c\" (oldest dropped at cap 2)", got.msg.Text)
	}
	got = waitForPush(t, target.pushes)
	if got.msg.Text != "d" || got.status != http.StatusOK {
		t.Errorf("second delivery after recovery = %q (status %d), want \"d\" with 200", got.msg.Text, got.status)
	}
}
