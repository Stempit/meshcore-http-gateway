package main

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

	// CLI scripting: cliReply/cliErr stand in for the device's answer,
	// cliCmd records the last command, the hooks let a test pause a device
	// round trip mid-flight (sendHook/cliHook) to observe serialization.
	cliReply string
	cliErr   error
	cliCmd   string
	sendHook func()
	cliHook  func()

	// Admin scripting: the error fields stand in for device outcomes, the
	// result fields for what the device would answer, and the records let
	// tests assert what was written to the device. adminHook lets a test
	// pause any admin round trip mid-flight (serialization tests).
	listContactsRes  []*meshcore.Contact
	listContactsErr  error
	listChannelsRes  []*meshcore.Channel
	listChannelsErr  error
	getByKeyRes      *meshcore.Contact
	getByKeyErr      error
	addContactErr    error
	removeContactErr error
	addChannelRes    *meshcore.Channel
	addChannelErr    error
	setChannelErr    error
	removeChannelErr error
	adminHook        func()

	addedContacts   []*meshcore.Contact
	removedKeys     []string
	getByKeyCalls   int
	addedChannels   []channelOp
	removedChannels []int

	// Status scripting: the cached snapshot GET /status serves.
	selfInfo  *meshcore.SelfInfo
	stats     *meshcore.CompanionRadioStats
	connected bool

	sentDMs     []sentDM
	sentChannel []sentChannel
}

// channelOp records one POST /channels device write so a test can assert
// which slot got which name and key. kind is "add", "create_private",
// "join_public", "join_hashtag" or "set"; RemoveChannel records separately
// in removedChannels.
type channelOp struct {
	kind  string
	name  string
	psk   []byte
	index int
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

func (f *fakeClient) GetSelfInfo() *meshcore.SelfInfo         { return f.selfInfo }
func (f *fakeClient) GetStats() *meshcore.CompanionRadioStats { return f.stats }
func (f *fakeClient) IsConnected() bool                       { return f.connected }

func (f *fakeClient) SendMessageWithRetry(contact *meshcore.Contact, text string, ack bool, _, _ int, _ time.Duration) error {
	if f.sendHook != nil {
		f.sendHook()
	}
	if f.dmErr != nil {
		return f.dmErr
	}
	f.sentDMs = append(f.sentDMs, sentDM{contact: contact, text: text, ack: ack})
	return nil
}

func (f *fakeClient) SendChannelMessageWithRetry(ch *meshcore.Channel, text string, ack bool, _, _ int, _ time.Duration) error {
	if f.sendHook != nil {
		f.sendHook()
	}
	if f.channelErr != nil {
		return f.channelErr
	}
	f.sentChannel = append(f.sentChannel, sentChannel{channel: ch, text: text, ack: ack})
	return nil
}

func (f *fakeClient) RunCLICommand(cmd string) (string, error) {
	if f.cliHook != nil {
		f.cliHook()
	}
	f.cliCmd = cmd
	if f.cliErr != nil {
		return "", f.cliErr
	}
	return f.cliReply, nil
}

// adminEntered marks an admin device round trip in the records and runs the
// serialization hook; every scripted admin method calls it first.
func (f *fakeClient) adminEntered() {
	if f.adminHook != nil {
		f.adminHook()
	}
}

func (f *fakeClient) ListContacts() ([]*meshcore.Contact, error) {
	f.adminEntered()
	return f.listContactsRes, f.listContactsErr
}

func (f *fakeClient) ListChannels() ([]*meshcore.Channel, error) {
	f.adminEntered()
	return f.listChannelsRes, f.listChannelsErr
}

func (f *fakeClient) GetContactByKey(pubKey []byte) (*meshcore.Contact, error) {
	f.adminEntered()
	f.getByKeyCalls++
	if f.getByKeyErr != nil {
		return nil, f.getByKeyErr
	}
	return f.getByKeyRes, nil
}

func (f *fakeClient) AddContact(ct *meshcore.Contact) error {
	f.adminEntered()
	if f.addContactErr != nil {
		return f.addContactErr
	}
	f.addedContacts = append(f.addedContacts, ct)
	return nil
}

func (f *fakeClient) RemoveContact(ct *meshcore.Contact) error {
	f.adminEntered()
	if f.removeContactErr != nil {
		return f.removeContactErr
	}
	f.removedKeys = append(f.removedKeys, hex.EncodeToString(ct.PublicKey))
	return nil
}

// addChannelResult is the shared body of the four channel-entry methods: run
// the admin hook, stand in for the device outcome, record the op.
func (f *fakeClient) addChannelResult(kind, name string, psk []byte) (*meshcore.Channel, error) {
	f.adminEntered()
	if f.addChannelErr != nil {
		return nil, f.addChannelErr
	}
	f.addedChannels = append(f.addedChannels, channelOp{kind: kind, name: name, psk: psk})
	return f.addChannelRes, nil
}

func (f *fakeClient) AddChannel(name string, psk []byte) (*meshcore.Channel, error) {
	return f.addChannelResult("add", name, psk)
}

func (f *fakeClient) CreatePrivateChannel(name string) (*meshcore.Channel, error) {
	return f.addChannelResult("create_private", name, nil)
}

func (f *fakeClient) JoinPublicChannel(name string) (*meshcore.Channel, error) {
	return f.addChannelResult("join_public", name, nil)
}

func (f *fakeClient) JoinHashtagChannel(name string) (*meshcore.Channel, error) {
	return f.addChannelResult("join_hashtag", name, nil)
}

func (f *fakeClient) SetChannel(index int, name string, psk []byte) error {
	f.adminEntered()
	if f.setChannelErr != nil {
		return f.setChannelErr
	}
	f.addedChannels = append(f.addedChannels, channelOp{kind: "set", name: name, psk: psk, index: index})
	return nil
}

func (f *fakeClient) RemoveChannel(index int) error {
	f.adminEntered()
	if f.removeChannelErr != nil {
		return f.removeChannelErr
	}
	f.removedChannels = append(f.removedChannels, index)
	return nil
}

func newTestServer(c deviceClient) *Server {
	return &Server{
		client:    c,
		inbox:     &companion.Inbox{},
		opts:      Options{Token: ""},
		startedAt: time.Now(),
		log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
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

func TestHandleCLI(t *testing.T) {
	fc := &fakeClient{cliReply: "> 869.618"}
	s := newTestServer(fc)

	rec := doRequest(s, "POST", "/cli", `{"command":"get tx"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("cli command: status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	// Raw bytes first: decodeBody drains the recorder's buffer.
	raw := rec.Body.String()
	if body := decodeBody(t, rec); body["reply"] != "> 869.618" {
		t.Errorf("reply not passed through verbatim: %v", body["reply"])
	}
	// The raw body must carry the real characters, not \u003e-style HTML
	// escapes — curl readers see the response as-is.
	if strings.Contains(raw, "\\u003e") || !strings.Contains(raw, "> 869.618") {
		t.Errorf("reply body HTML-escaped or wrong: %s", raw)
	}
	if fc.cliCmd != "get tx" {
		t.Errorf("command not forwarded: %q", fc.cliCmd)
	}

	cases := []struct {
		name string
		body string
	}{
		{"invalid JSON", "{nope"},
		{"missing command", `{}`},
		{"whitespace command", `{"command":"   "}`},
		{"too short", `{"command":"v"}`},
		{"too long", `{"command":"` + strings.Repeat("x", 161) + `"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doRequest(s, "POST", "/cli", tc.body, nil)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("body %q: status = %d, want 400", tc.body, rec.Code)
			}
		})
	}

	fc.cliErr = &meshcore.DeviceError{Code: 0x01}
	rec = doRequest(s, "POST", "/cli", `{"command":"ver"}`, nil)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("device-rejected command: status = %d, want 502", rec.Code)
	}
	// A device rejection reports the device's own error, unprefixed —
	// distinct from the default branch's "cli command failed: ..." wrapper.
	if body := rec.Body.String(); !strings.Contains(body, "device error 1 (unsupported_cmd)") || strings.Contains(body, "cli command failed") {
		t.Errorf("device rejection body must surface the device error verbatim, got %s", body)
	}

	fc.cliErr = fmt.Errorf("cli: %w", meshcore.ErrNoResponse)
	rec = doRequest(s, "POST", "/cli", `{"command":"ver"}`, nil)
	if rec.Code != http.StatusGatewayTimeout {
		t.Errorf("unanswered command: status = %d, want 504", rec.Code)
	}
}

// TestHandleCLIWaitsForInFlightSend pins the serialization contract: the
// device path is shared, so a /cli issued while /send is in flight must queue
// behind it instead of interleaving command frames with the send's
// confirmation window.
func TestHandleCLIWaitsForInFlightSend(t *testing.T) {
	yorick := &meshcore.Contact{Name: "Yorick 💀", PublicKey: []byte{1, 2, 3, 4}}
	fc := &fakeClient{
		contacts: map[string]*meshcore.Contact{"yorick": yorick},
		cliReply: "> 869.618",
	}
	s := newTestServer(fc)

	sendStarted := make(chan struct{})
	releaseSend := make(chan struct{})
	var mu sync.Mutex
	var events []string
	fc.sendHook = func() {
		mu.Lock()
		events = append(events, "send")
		mu.Unlock()
		close(sendStarted)
		<-releaseSend
	}
	fc.cliHook = func() {
		mu.Lock()
		events = append(events, "cli")
		mu.Unlock()
	}

	sendDone := make(chan struct{})
	go func() {
		doRequest(s, "POST", "/send", `{"type":"dm","to":"Yorick 💀","text":"hi"}`, nil)
		close(sendDone)
	}()
	<-sendStarted

	cliDone := make(chan struct{})
	go func() {
		doRequest(s, "POST", "/cli", `{"command":"get tx"}`, nil)
		close(cliDone)
	}()
	select {
	case <-cliDone:
		t.Fatal("/cli completed while /send was still in flight")
	case <-time.After(50 * time.Millisecond):
	}

	close(releaseSend)
	<-sendDone
	<-cliDone

	mu.Lock()
	defer mu.Unlock()
	if len(events) != 2 || events[0] != "send" || events[1] != "cli" {
		t.Errorf("device round trips interleaved: events = %v, want [send cli]", events)
	}
}

func TestHandleStatus(t *testing.T) {
	fc := &fakeClient{
		connected: true,
		selfInfo: &meshcore.SelfInfo{
			Name: "Bot-AB12", Model: "Heltec V3", FirmwareVersion: "1.8.0",
			FreqHz: 869618000, BwHz: 250000, Sf: 9, Cr: 5,
			TxPower: 14, MaxTxPower: 22, Latitude: 55.75, Longitude: 37.61,
		},
		stats: &meshcore.CompanionRadioStats{
			BatteryMv: 3980, UptimeSecs: 12345, LastRssi: -98,
			LastUpdate: time.Now().Add(-2 * time.Minute),
		},
	}
	s := newTestServer(fc)
	s.opts.Transport = "bt"
	s.clock = startupClock{
		DeviceTime: time.Unix(1762612345, 0).UTC(),
		SampledAt:  time.Now(),
	}

	rec := doRequest(s, "GET", "/status", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("/status: status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	body := decodeBody(t, rec)
	if body["connected"] != true {
		t.Errorf("connected = %v, want true", body["connected"])
	}
	if body["transport"] != "bt" {
		t.Errorf("transport = %v, want the configured connection type", body["transport"])
	}
	dev, ok := body["device"].(map[string]any)
	if !ok || dev["name"] != "Bot-AB12" || dev["model"] != "Heltec V3" || dev["firmware"] != "1.8.0" {
		t.Errorf("device block not built from self info: %v", dev)
	}
	st, ok := body["stats"].(map[string]any)
	if !ok {
		t.Fatalf("stats block missing: %v", body)
	}
	if st["battery_mv"] != float64(3980) {
		t.Errorf("battery_mv = %v, want 3980", st["battery_mv"])
	}
	if age, ok := st["age_secs"].(float64); !ok || age < 119 || age > 125 {
		t.Errorf("age_secs = %v, want ~120 (the scripted stats staleness)", st["age_secs"])
	}
	if _, ok := body["device_time"]; !ok {
		t.Error("device_time missing although the startup clock was sampled")
	}
	if _, ok := body["device_time_sampled_at"]; !ok {
		t.Error("device_time_sampled_at missing although the startup clock was sampled")
	}
	if _, ok := body["started_at"]; !ok {
		t.Error("started_at missing")
	}

	// A bare server omits everything optional instead of emitting zeroes.
	bare := newTestServer(&fakeClient{})
	rec = doRequest(bare, "GET", "/status", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("bare /status: status = %d, want 200", rec.Code)
	}
	body = decodeBody(t, rec)
	if body["connected"] != false {
		t.Errorf("bare connected = %v, want false", body["connected"])
	}
	for _, key := range []string{"device", "stats", "device_time", "device_time_sampled_at"} {
		if _, ok := body[key]; ok {
			t.Errorf("bare server: %q must be omitted, got %v", key, body[key])
		}
	}

	// A future LastUpdate (host clock jumped backwards since the sample)
	// clamps to a zero age, never a negative one.
	fc.stats = &meshcore.CompanionRadioStats{BatteryMv: 1, LastUpdate: time.Now().Add(time.Hour)}
	rec = doRequest(s, "GET", "/status", "", nil)
	body = decodeBody(t, rec)
	st, ok = body["stats"].(map[string]any)
	if !ok {
		t.Fatalf("stats block missing: %v", body)
	}
	if age, ok := st["age_secs"].(float64); !ok || age != 0 {
		t.Errorf("future LastUpdate: age_secs = %v, want 0", st["age_secs"])
	}
}
