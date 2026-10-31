package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stempit/meshcore-go"
	"github.com/stempit/meshcore-kit/companion"
)

// fakeClient is a scripted deviceClient: contact/channel maps stand in for
// the radio's directory, the error fields stand in for radio outcomes. Its
// lookup semantics mirror meshcore-go: GetChannelByName skips empty slots,
// FindContactByName returns the first name match.
type fakeClient struct {
	contacts map[string]*meshcore.Contact
	channels map[int]*meshcore.Channel

	dmErr      error
	channelErr error

	sentDMs     []sentDM
	sentChannel []sentChannel
}

type sentDM struct {
	contact *meshcore.Contact
	text    string
	ack     bool
}

type sentChannel struct {
	channel *meshcore.Channel
	text    string
	ack     bool
}

func (f *fakeClient) GetContacts() map[string]*meshcore.Contact { return f.contacts }
func (f *fakeClient) GetChannels() map[int]*meshcore.Channel    { return f.channels }

func (f *fakeClient) FindContactByName(name string) *meshcore.Contact {
	for _, c := range f.contacts {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func (f *fakeClient) FindContactByKeyPrefix(prefix []byte) *meshcore.Contact {
	for _, c := range f.contacts {
		if len(c.PublicKey) >= len(prefix) && string(c.PublicKey[:len(prefix)]) == string(prefix) {
			return c
		}
	}
	return nil
}

func (f *fakeClient) GetChannelByName(name string) *meshcore.Channel {
	for _, ch := range f.channels {
		if !ch.IsEmpty && ch.Name == name {
			return ch
		}
	}
	return nil
}

func (f *fakeClient) SendMessageWithRetry(contact *meshcore.Contact, text string, ack bool, _, _ int, _ time.Duration) error {
	if f.dmErr != nil {
		return f.dmErr
	}
	f.sentDMs = append(f.sentDMs, sentDM{contact: contact, text: text, ack: ack})
	return nil
}

func (f *fakeClient) SendChannelMessageWithRetry(ch *meshcore.Channel, text string, ack bool, _, _ int, _ time.Duration) error {
	if f.channelErr != nil {
		return f.channelErr
	}
	f.sentChannel = append(f.sentChannel, sentChannel{channel: ch, text: text, ack: ack})
	return nil
}

func newTestServer(c deviceClient) *Server {
	return &Server{
		client: c,
		inbox:  &companion.Inbox{},
		opts:   Options{Token: ""},
		log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func doRequest(s *Server, method, target, body string, headers map[string]string) *httptest.ResponseRecorder {
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, target, strings.NewReader(body))
	} else {
		req = httptest.NewRequest(method, target, nil)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	return rec
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&m); err != nil {
		t.Fatalf("decoding response body %q: %v", rec.Body.String(), err)
	}
	return m
}

func TestWriteSendResultMapping(t *testing.T) {
	s := newTestServer(&fakeClient{})
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"success", nil, http.StatusOK},
		{"no confirmation", fmt.Errorf("dm to X: %w", meshcore.ErrNoConfirmation), http.StatusGatewayTimeout},
		{"device rejected", fmt.Errorf("dm to X: %w", meshcore.ErrDeviceRejected), http.StatusBadGateway},
		{"plain transport error", errors.New("transport dead"), http.StatusBadGateway},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			s.writeSendResult(rec, tc.err)
			if rec.Code != tc.want {
				t.Errorf("err=%v: status = %d, want %d", tc.err, rec.Code, tc.want)
			}
		})
	}
}

func TestWithAuth(t *testing.T) {
	cases := []struct {
		name   string
		token  string
		header string
		want   int
	}{
		{"no token configured passes", "", "", http.StatusOK},
		{"correct token passes", "s3cret", "Bearer s3cret", http.StatusOK},
		{"missing header", "s3cret", "", http.StatusUnauthorized},
		{"wrong scheme", "s3cret", "Token s3cret", http.StatusUnauthorized},
		{"wrong token", "s3cret", "Bearer nope", http.StatusUnauthorized},
		{"token prefix only", "s3cret", "Bearer s3cr", http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(&fakeClient{})
			s.opts.Token = tc.token
			rec := doRequest(s, "GET", "/get", "", map[string]string{"Authorization": tc.header})
			if rec.Code != tc.want {
				t.Errorf("token=%q header=%q: status = %d, want %d", tc.token, tc.header, rec.Code, tc.want)
			}
		})
	}
}

func TestWithAuthGuardsUnknownPaths(t *testing.T) {
	s := newTestServer(&fakeClient{})
	s.opts.Token = "s3cret"

	rec := doRequest(s, "GET", "/nope", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("unknown path without token: status = %d, want 401 (auth must wrap routing)", rec.Code)
	}
	rec = doRequest(s, "GET", "/nope", "", map[string]string{"Authorization": "Bearer s3cret"})
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown path with token: status = %d, want 404", rec.Code)
	}
}

func TestHandleGetDrainsInbox(t *testing.T) {
	s := newTestServer(&fakeClient{})

	rec := doRequest(s, "GET", "/get", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("empty /get: status = %d, want 200", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "null") {
		t.Errorf("empty inbox must encode as [], got %q", rec.Body.String())
	}

	s.inbox.Add(companion.Message{Type: "dm", Sender: "Yorick", Text: "ping"})
	rec = doRequest(s, "GET", "/get", "", nil)
	body := decodeBody(t, rec)
	if msgs, ok := body["messages"].([]any); !ok || len(msgs) != 1 {
		t.Errorf("/get after Add: want 1 message, got %v", body["messages"])
	}

	rec = doRequest(s, "GET", "/get", "", nil)
	body = decodeBody(t, rec)
	if msgs, ok := body["messages"].([]any); !ok || len(msgs) != 0 {
		t.Errorf("second /get must drain to empty, got %v", body["messages"])
	}
}

func TestHandleMessagesCursor(t *testing.T) {
	s := newTestServer(&fakeClient{})
	first := s.inbox.Add(companion.Message{Type: "dm", Text: "one"})
	second := s.inbox.Add(companion.Message{Type: "channel", Text: "two"})

	rec := doRequest(s, "GET", "/messages", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("/messages: status = %d, want 200", rec.Code)
	}
	body := decodeBody(t, rec)
	if msgs, ok := body["messages"].([]any); !ok || len(msgs) != 2 {
		t.Errorf("no cursor: want both messages, got %v", body["messages"])
	}
	if lastID, ok := body["last_id"].(float64); !ok || lastID != float64(second.ID) {
		t.Errorf("last_id = %v, want %d", body["last_id"], second.ID)
	}

	rec = doRequest(s, "GET", fmt.Sprintf("/messages?after=%d", first.ID), "", nil)
	body = decodeBody(t, rec)
	if msgs, ok := body["messages"].([]any); !ok || len(msgs) != 1 {
		t.Errorf("after=%d: want only the newer message, got %v", first.ID, body["messages"])
	}

	rec = doRequest(s, "GET", "/messages?after=99999", "", nil)
	body = decodeBody(t, rec)
	if msgs, ok := body["messages"].([]any); !ok || len(msgs) != 0 {
		t.Errorf("after beyond last_id: want empty, got %v", body["messages"])
	}

	rec = doRequest(s, "GET", "/messages?after=abc", "", nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("non-integer after: status = %d, want 400", rec.Code)
	}
}

func TestResolveContact(t *testing.T) {
	fc := &fakeClient{contacts: map[string]*meshcore.Contact{
		"yorick": {Name: "Yorick 💀", PublicKey: []byte{0xaa, 0xbb, 0xcc, 0xdd}},
		"aabb":   {Name: "aabb", PublicKey: []byte{0xff, 0xff}},
	}}
	s := newTestServer(fc)

	if got := s.resolveContact("Yorick 💀"); got == nil || got.Name != "Yorick 💀" {
		t.Errorf("exact name did not resolve: %v", got)
	}
	if got := s.resolveContact("aabb"); got == nil || got.PublicKey[0] != 0xff {
		t.Errorf("name lookup must beat hex-prefix: got %v", got)
	}
	if got := s.resolveContact("aabbcc"); got == nil || got.Name != "Yorick 💀" {
		t.Errorf("hex prefix of full key did not resolve: %v", got)
	}
	if got := s.resolveContact("AABBCC"); got == nil || got.Name != "Yorick 💀" {
		t.Errorf("uppercase hex must resolve (lowercased): %v", got)
	}
	for _, bad := range []string{"aab", "zzzz", "nobody", ""} {
		if got := s.resolveContact(bad); got != nil {
			t.Errorf("%q must not resolve, got %v", bad, got)
		}
	}
}

func TestResolveChannel(t *testing.T) {
	fc := &fakeClient{channels: map[int]*meshcore.Channel{
		0: {Index: 0, Name: "public"},
		2: {Index: 2, Name: "private"},
		3: {Index: 3, IsEmpty: true},
	}}
	s := newTestServer(fc)

	if got := s.resolveChannel("public"); got == nil || got.Index != 0 {
		t.Errorf("channel by name did not resolve: %v", got)
	}
	if got := s.resolveChannel("2"); got == nil || got.Index != 2 {
		t.Errorf("channel by index did not resolve: %v", got)
	}
	if got := s.resolveChannel("0"); got == nil || got.Index != 0 {
		t.Errorf("channel index 0 did not resolve: %v", got)
	}
	for _, bad := range []string{"3", "-1", "99", "ghost", ""} {
		if got := s.resolveChannel(bad); got != nil {
			t.Errorf("%q must not resolve, got %v", bad, got)
		}
	}
}

func TestHandleSendValidation(t *testing.T) {
	s := newTestServer(&fakeClient{})
	cases := []struct {
		name string
		body string
	}{
		{"invalid JSON", "{not json"},
		{"missing to", `{"type":"dm","text":"hi"}`},
		{"missing text", `{"type":"dm","to":"Yorick"}`},
		{"whitespace text", `{"type":"dm","to":"Yorick","text":"   "}`},
		{"unknown type", `{"type":"email","to":"x","text":"y"}`},
		{"oversized body", `{"type":"dm","to":"x","text":"` + strings.Repeat("x", 66*1024) + `"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doRequest(s, "POST", "/send", tc.body, nil)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("body %q: status = %d, want 400", tc.body, rec.Code)
			}
		})
	}
}

func TestHandleSendDM(t *testing.T) {
	yorick := &meshcore.Contact{Name: "Yorick 💀", PublicKey: []byte{1, 2, 3, 4}}
	fc := &fakeClient{contacts: map[string]*meshcore.Contact{"yorick": yorick}}
	s := newTestServer(fc)

	rec := doRequest(s, "POST", "/send", `{"type":"dm","to":"Yorick 💀","text":"copy that","ack":true}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("dm send: status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if len(fc.sentDMs) != 1 || fc.sentDMs[0].contact != yorick || fc.sentDMs[0].text != "copy that" || !fc.sentDMs[0].ack {
		t.Errorf("send not forwarded as expected: %+v", fc.sentDMs)
	}

	rec = doRequest(s, "POST", "/send", `{"type":"dm","to":"ghost","text":"hi"}`, nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown contact: status = %d, want 404", rec.Code)
	}

	fc.dmErr = fmt.Errorf("dm to ghost: %w", meshcore.ErrNoConfirmation)
	rec = doRequest(s, "POST", "/send", `{"type":"dm","to":"Yorick 💀","text":"hi"}`, nil)
	if rec.Code != http.StatusGatewayTimeout {
		t.Errorf("unconfirmed dm: status = %d, want 504", rec.Code)
	}
}

func TestHandleSendChannel(t *testing.T) {
	public := &meshcore.Channel{Index: 0, Name: "public"}
	fc := &fakeClient{channels: map[int]*meshcore.Channel{0: public}}
	s := newTestServer(fc)
	s.opts.ChannelTransmissions = 5

	rec := doRequest(s, "POST", "/send", `{"type":"channel","to":"public","text":"hello"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("channel send: status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if len(fc.sentChannel) != 1 || fc.sentChannel[0].channel != public || fc.sentChannel[0].text != "hello" {
		t.Errorf("send not forwarded as expected: %+v", fc.sentChannel)
	}

	rec = doRequest(s, "POST", "/send", `{"type":"channel","to":"0","text":"hello"}`, nil)
	if rec.Code != http.StatusOK {
		t.Errorf("channel by index: status = %d, want 200", rec.Code)
	}

	rec = doRequest(s, "POST", "/send", `{"type":"channel","to":"99","text":"hello"}`, nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown channel: status = %d, want 404", rec.Code)
	}

	fc.channelErr = errors.New("device exploded")
	rec = doRequest(s, "POST", "/send", `{"type":"channel","to":"public","text":"hello"}`, nil)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("channel failure: status = %d, want 502", rec.Code)
	}
}

func TestHandleContactsAndChannels(t *testing.T) {
	now := time.Now()
	fc := &fakeClient{
		contacts: map[string]*meshcore.Contact{
			"b": {Name: "Beta", PublicKey: []byte{0xde, 0xad}, LastSeen: now},
			"a": {Name: "Alpha", PublicKey: []byte{0xbe, 0xef}},
		},
		channels: map[int]*meshcore.Channel{
			2: {Index: 2, Name: "private"},
			0: {Index: 0, Name: "public"},
			3: {Index: 3, IsEmpty: true},
		},
	}
	s := newTestServer(fc)

	rec := doRequest(s, "GET", "/contacts", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("/contacts: status = %d, want 200", rec.Code)
	}
	body := decodeBody(t, rec)
	contacts, ok := body["contacts"].([]any)
	if !ok || len(contacts) != 2 {
		t.Fatalf("/contacts: want 2 entries, got %v", body["contacts"])
	}
	first, _ := contacts[0].(map[string]any)
	if first["name"] != "Alpha" {
		t.Errorf("/contacts not sorted by name: first = %v", first["name"])
	}
	if first["key"] != "beef" {
		t.Errorf("key must be lowercase hex, got %v", first["key"])
	}
	if _, ok := first["last_seen"]; !ok {
		t.Error("last_seen field missing")
	}

	rec = doRequest(s, "GET", "/channels", "", nil)
	body = decodeBody(t, rec)
	channels, ok := body["channels"].([]any)
	if !ok || len(channels) != 2 {
		t.Fatalf("/channels: want 2 non-empty entries, got %v", body["channels"])
	}
	c0, _ := channels[0].(map[string]any)
	c1, _ := channels[1].(map[string]any)
	if c0["index"] != float64(0) || c0["name"] != "public" || c1["index"] != float64(2) {
		t.Errorf("/channels not sorted by index with empty slots skipped: %v %v", c0, c1)
	}
}
