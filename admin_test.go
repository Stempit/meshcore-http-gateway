package main

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stempit/meshcore-go"
)

// testKeyHex is a full 32-byte public key in hex (0xab × 32); testPSKHex is a
// 16-byte channel key in hex (0x11 × 16) — the exact shapes the admin
// endpoints take and hand back.
var (
	testKeyHex = strings.Repeat("ab", 32)
	testPSKHex = strings.Repeat("11", 16)
)

func fptr(v float64) *float64 { return &v }

func TestWantsFullListing(t *testing.T) {
	cases := []struct {
		raw  string
		want bool
	}{
		{"/contacts", false},
		{"/contacts?full", true},
		{"/contacts?full=", true},
		{"/contacts?full=true", true},
		{"/contacts?full=1", true},
		{"/contacts?full=false", false},
		{"/contacts?full=0", false},
	}
	for _, tc := range cases {
		r := httptest.NewRequest(http.MethodGet, tc.raw, nil)
		if got := wantsFullListing(r); got != tc.want {
			t.Errorf("%s: wantsFullListing = %v, want %v", tc.raw, got, tc.want)
		}
	}
}

func TestHandleContactAdd(t *testing.T) {
	fc := &fakeClient{}
	s := newTestServer(fc)

	body := fmt.Sprintf(`{"key":%q,"name":"Yorick 💀","type":"sensor","flags":["favorite","tele_env"],"lat":55.75,"lon":37.61}`, testKeyHex)
	rec := doRequest(s, "POST", "/contacts", body, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("add: status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if len(fc.addedContacts) != 1 {
		t.Fatalf("add did not reach the device: %+v", fc.addedContacts)
	}
	ct := fc.addedContacts[0]
	if ct.Name != "Yorick 💀" || ct.Type != meshcore.AdvTypeSensor {
		t.Errorf("contact not forwarded as expected: %+v", ct)
	}
	if ct.Flags != meshcore.ContactFlagFavorite|meshcore.ContactFlagTeleEnv {
		t.Errorf("flags = %d, want favorite|tele_env", ct.Flags)
	}
	if ct.Latitude == nil || *ct.Latitude != 55.75 || ct.Longitude == nil || *ct.Longitude != 37.61 {
		t.Errorf("location not forwarded: %v %v", ct.Latitude, ct.Longitude)
	}
	if got := decodeBody(t, rec)["key"]; got != testKeyHex {
		t.Errorf("response key = %v, want the full hex key", got)
	}

	// Defaults: an empty type is chat, no flags is an empty mask, no location.
	rec = doRequest(s, "POST", "/contacts", fmt.Sprintf(`{"key":%q,"name":"Plain"}`, testKeyHex), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("default add: status = %d, want 200", rec.Code)
	}
	ct = fc.addedContacts[1]
	if ct.Type != meshcore.AdvTypeChat || ct.Flags != 0 || ct.Latitude != nil || ct.Longitude != nil {
		t.Errorf("defaults not applied: %+v", ct)
	}

	// Type is trimmed like flags are: " chat " must not be a 400.
	rec = doRequest(s, "POST", "/contacts", fmt.Sprintf(`{"key":%q,"name":"Padded","type":" chat "}`, testKeyHex), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("padded type: status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if ct = fc.addedContacts[2]; ct.Type != meshcore.AdvTypeChat {
		t.Errorf("padded type not resolved: %+v", ct)
	}

	// Boundary: exactly 31 bytes fits the on-device name field.
	rec = doRequest(s, "POST", "/contacts", `{"key":"`+testKeyHex+`","name":"`+strings.Repeat("x", 31)+`"}`, nil)
	if rec.Code != http.StatusOK {
		t.Errorf("31-byte name: status = %d, want 200", rec.Code)
	}

	// Device outcomes: storage full is a conflict, silence is a timeout.
	fc.addContactErr = &meshcore.DeviceError{Code: 0x03} // table_full
	rec = doRequest(s, "POST", "/contacts", fmt.Sprintf(`{"key":%q,"name":"X"}`, testKeyHex), nil)
	if rec.Code != http.StatusConflict {
		t.Errorf("table_full: status = %d, want 409", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "table_full") {
		t.Errorf("conflict body must name the device error, got %s", body)
	}
	fc.addContactErr = fmt.Errorf("admin: %w", meshcore.ErrNoResponse)
	rec = doRequest(s, "POST", "/contacts", fmt.Sprintf(`{"key":%q,"name":"X"}`, testKeyHex), nil)
	if rec.Code != http.StatusGatewayTimeout {
		t.Errorf("unanswered: status = %d, want 504", rec.Code)
	}
}

func TestHandleContactAddValidation(t *testing.T) {
	s := newTestServer(&fakeClient{})
	cases := []struct{ name, body string }{
		{"invalid JSON", "{nope"},
		{"missing key", `{"name":"X"}`},
		{"short key", `{"key":"aabb","name":"X"}`},
		{"non-hex key", `{"key":"` + strings.Repeat("zz", 32) + `","name":"X"}`},
		{"missing name", `{"key":"` + testKeyHex + `"}`},
		{"whitespace name", `{"key":"` + testKeyHex + `","name":"   "}`},
		{"overlong name", `{"key":"` + testKeyHex + `","name":"` + strings.Repeat("x", 32) + `"}`},
		{"unknown type", `{"key":"` + testKeyHex + `","name":"X","type":"router"}`},
		{"unknown flag", `{"key":"` + testKeyHex + `","name":"X","flags":["vip"]}`},
		{"lat without lon", `{"key":"` + testKeyHex + `","name":"X","lat":55.7}`},
		{"lon without lat", `{"key":"` + testKeyHex + `","name":"X","lon":37.6}`},
		{"lat out of range", `{"key":"` + testKeyHex + `","name":"X","lat":91,"lon":37.6}`},
		{"lon out of range", `{"key":"` + testKeyHex + `","name":"X","lat":55.7,"lon":181}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doRequest(s, "POST", "/contacts", tc.body, nil)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("body %q: status = %d, want 400", tc.body, rec.Code)
			}
		})
	}
}

func TestHandleContactGet(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = 0xab
	}
	yorick := &meshcore.Contact{Name: "Yorick 💀", PublicKey: key, Type: meshcore.AdvTypeRoom}
	fc := &fakeClient{getByKeyRes: yorick}
	s := newTestServer(fc)

	rec := doRequest(s, "GET", "/contacts/"+testKeyHex, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get: status = %d (body %s)", rec.Code, rec.Body.String())
	}
	if fc.getByKeyCalls != 1 {
		t.Fatal("get did not reach the device")
	}
	body := decodeBody(t, rec)
	if body["name"] != "Yorick 💀" || body["type_name"] != "room" || body["key"] != testKeyHex {
		t.Errorf("details not rendered: %v", body)
	}
	if flags, ok := body["flags"].([]any); !ok || len(flags) != 0 {
		t.Errorf("flagless contact: flags = %v, want an empty array", body["flags"])
	}
	if _, ok := body["lat"]; ok {
		t.Error("lat must be omitted for a locationless contact")
	}
	if _, ok := body["last_message_at"]; ok {
		t.Error("last_message_at must be omitted when no message was exchanged")
	}

	rec = doRequest(s, "GET", "/contacts/not-hex", "", nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad key: status = %d, want 400", rec.Code)
	}

	fc.getByKeyErr = &meshcore.DeviceError{Code: 0x02} // not_found
	rec = doRequest(s, "GET", "/contacts/"+testKeyHex, "", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("not_found: status = %d, want 404", rec.Code)
	}

	fc.getByKeyErr = nil
	fc.getByKeyRes = nil // device answered, but the entry is gone anyway
	rec = doRequest(s, "GET", "/contacts/"+testKeyHex, "", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("nil contact: status = %d, want 404", rec.Code)
	}

	fc.getByKeyErr = fmt.Errorf("admin: %w", meshcore.ErrNoResponse)
	rec = doRequest(s, "GET", "/contacts/"+testKeyHex, "", nil)
	if rec.Code != http.StatusGatewayTimeout {
		t.Errorf("unanswered: status = %d, want 504", rec.Code)
	}
}

func TestHandleContactDelete(t *testing.T) {
	fc := &fakeClient{}
	s := newTestServer(fc)

	rec := doRequest(s, "DELETE", "/contacts/"+testKeyHex, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: status = %d (body %s)", rec.Code, rec.Body.String())
	}
	if len(fc.removedKeys) != 1 || fc.removedKeys[0] != testKeyHex {
		t.Errorf("remove not forwarded with the full key: %v", fc.removedKeys)
	}

	rec = doRequest(s, "DELETE", "/contacts/zz", "", nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad key: status = %d, want 400", rec.Code)
	}

	fc.removeContactErr = &meshcore.DeviceError{Code: 0x02} // not_found
	rec = doRequest(s, "DELETE", "/contacts/"+testKeyHex, "", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("not_found: status = %d, want 404", rec.Code)
	}
}

func TestHandleContactsFull(t *testing.T) {
	lastMsg := time.Unix(1762600000, 0).UTC()
	fc := &fakeClient{listContactsRes: []*meshcore.Contact{
		{
			Name: "Yorick 💀", PublicKey: []byte{0xab, 0xcd}, Type: meshcore.AdvTypeRepeater,
			Flags:      meshcore.ContactFlagFavorite | meshcore.ContactFlagTeleEnv,
			PathLength: 2, Path: []byte{0xde, 0xad},
			Latitude: fptr(55.75), Longitude: fptr(37.61),
			LastSeen: lastMsg, LastMessageAt: lastMsg, LastMod: 1700000000,
		},
	}}
	s := newTestServer(fc)

	rec := doRequest(s, "GET", "/contacts?full", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("full listing: status = %d (body %s)", rec.Code, rec.Body.String())
	}
	body := decodeBody(t, rec)
	contacts, ok := body["contacts"].([]any)
	if !ok || len(contacts) != 1 {
		t.Fatalf("full listing: want 1 entry, got %v", body["contacts"])
	}
	ct, _ := contacts[0].(map[string]any)
	if ct["type_name"] != "repeater" || ct["type"] != float64(meshcore.AdvTypeRepeater) {
		t.Errorf("type not rendered: %v", ct)
	}
	if flags, _ := ct["flags"].([]any); len(flags) != 2 || flags[0] != "favorite" || flags[1] != "tele_env" {
		t.Errorf("flags = %v, want [favorite tele_env] (sorted)", ct["flags"])
	}
	if ct["key"] != "abcd" || ct["path_length"] != float64(2) || ct["path_hex"] != "dead" {
		t.Errorf("key/path not rendered: %v", ct)
	}
	if ct["lat"] != 55.75 || ct["lon"] != 37.61 {
		t.Errorf("location not rendered: %v", ct)
	}
	for _, field := range []string{"last_seen", "last_message_at", "is_active", "last_mod"} {
		if _, ok := ct[field]; !ok {
			t.Errorf("field %q missing from the full listing", field)
		}
	}

	// A listing failure surfaces as a device failure.
	fc.listContactsErr = errors.New("transport dead")
	rec = doRequest(s, "GET", "/contacts?full", "", nil)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("device failure: status = %d, want 502", rec.Code)
	}

	// ?full=false keeps the cheap cached directory shape (no extra fields).
	fc.contacts = map[string]*meshcore.Contact{
		"a": {Name: "Alpha", PublicKey: []byte{0xbe, 0xef}},
	}
	rec = doRequest(s, "GET", "/contacts?full=false", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("cached listing: status = %d, want 200", rec.Code)
	}
	body = decodeBody(t, rec)
	contacts, _ = body["contacts"].([]any)
	first, _ := contacts[0].(map[string]any)
	if _, ok := first["type_name"]; ok {
		t.Error("cached listing must keep the directory shape (no type_name)")
	}
	if first["key"] != "beef" {
		t.Errorf("cached listing key = %v, want beef", first["key"])
	}
}

func TestHandleChannelsFull(t *testing.T) {
	psk := make([]byte, 16)
	for i := range psk {
		psk[i] = 0x11
	}
	fc := &fakeClient{listChannelsRes: []*meshcore.Channel{
		{Index: 0, Name: "public", PSK: make([]byte, 16)},
		{Index: 2, Name: "team", PSK: psk},
		{Index: 3, IsEmpty: true, PSK: make([]byte, 16)},
	}}
	s := newTestServer(fc)

	rec := doRequest(s, "GET", "/channels?full", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("full listing: status = %d (body %s)", rec.Code, rec.Body.String())
	}
	body := decodeBody(t, rec)
	channels, ok := body["channels"].([]any)
	if !ok || len(channels) != 3 {
		t.Fatalf("want all 3 slots including the empty one, got %v", body["channels"])
	}
	team, _ := channels[1].(map[string]any)
	if team["index"] != float64(2) || team["name"] != "team" || team["psk_hex"] != testPSKHex || team["is_empty"] != false {
		t.Errorf("team slot not rendered with its key: %v", team)
	}
	wantHash := sha256.Sum256(psk)
	if team["hash_byte"] != float64(wantHash[0]) {
		t.Errorf("hash_byte = %v, want %d (first SHA256(PSK) byte)", team["hash_byte"], wantHash[0])
	}
	empty, _ := channels[2].(map[string]any)
	if empty["is_empty"] != true {
		t.Errorf("empty slot lost its flag: %v", empty)
	}

	fc.listChannelsErr = errors.New("transport dead")
	rec = doRequest(s, "GET", "/channels?full", "", nil)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("device failure: status = %d, want 502", rec.Code)
	}
}

func TestHandleChannelAdd(t *testing.T) {
	fc := &fakeClient{}
	s := newTestServer(fc)

	teamPSK := make([]byte, 16)
	for i := range teamPSK {
		teamPSK[i] = 0x22
	}

	// create_private: the name is forwarded and the generated key comes back,
	// ready to share (a QR secret).
	fc.addChannelRes = &meshcore.Channel{Index: 1, Name: "Team", PSK: teamPSK}
	rec := doRequest(s, "POST", "/channels", `{"action":"create_private","name":"Team"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("create_private: status = %d (body %s)", rec.Code, rec.Body.String())
	}
	if len(fc.addedChannels) != 1 || fc.addedChannels[0].kind != "create_private" || fc.addedChannels[0].name != "Team" {
		t.Fatalf("create_private not forwarded: %+v", fc.addedChannels)
	}
	if body := decodeBody(t, rec); body["index"] != float64(1) || body["psk_hex"] != strings.Repeat("22", 16) {
		t.Errorf("response must carry the slot with its key: %v", body)
	}

	// add: an explicit PSK joins an existing private channel.
	joinedPSK := make([]byte, 16)
	for i := range joinedPSK {
		joinedPSK[i] = 0x33
	}
	fc.addChannelRes = &meshcore.Channel{Index: 2, Name: "Joined", PSK: joinedPSK}
	rec = doRequest(s, "POST", "/channels", `{"action":"add","name":"Joined","psk":"`+testPSKHex+`"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("add: status = %d (body %s)", rec.Code, rec.Body.String())
	}
	op := fc.addedChannels[1]
	if op.kind != "add" || op.name != "Joined" || len(op.psk) != 16 || op.psk[0] != 0x11 {
		t.Errorf("add not forwarded as expected: %+v", op)
	}

	// join_public: the name is optional — the library defaults the alias.
	rec = doRequest(s, "POST", "/channels", `{"action":"join_public"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("join_public: status = %d (body %s)", rec.Code, rec.Body.String())
	}
	if op := fc.addedChannels[2]; op.kind != "join_public" || op.name != "" {
		t.Errorf("join_public not forwarded: %+v", op)
	}

	// join_hashtag: the name passes through, leading '#' included.
	rec = doRequest(s, "POST", "/channels", `{"action":"join_hashtag","name":"#test"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("join_hashtag: status = %d (body %s)", rec.Code, rec.Body.String())
	}
	if op := fc.addedChannels[3]; op.kind != "join_hashtag" || op.name != "#test" {
		t.Errorf("join_hashtag not forwarded: %+v", op)
	}

	// set: a direct slot write.
	rec = doRequest(s, "POST", "/channels", `{"action":"set","index":3,"name":"Moved","psk":"`+testPSKHex+`"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("set: status = %d (body %s)", rec.Code, rec.Body.String())
	}
	op = fc.addedChannels[4]
	if op.kind != "set" || op.index != 3 || op.name != "Moved" || len(op.psk) != 16 {
		t.Errorf("set not forwarded as expected: %+v", op)
	}
	if body := decodeBody(t, rec); body["index"] != float64(3) || body["name"] != "Moved" {
		t.Errorf("set response: %v", body)
	}

	// Device outcomes: no free slot is a conflict, a rejection is a 502.
	fc.addChannelErr = meshcore.ErrNoEmptyChannelSlot
	rec = doRequest(s, "POST", "/channels", `{"action":"create_private","name":"X"}`, nil)
	if rec.Code != http.StatusConflict {
		t.Errorf("no empty slot: status = %d, want 409", rec.Code)
	}
	fc.addChannelErr = &meshcore.DeviceError{Code: 0x01} // unsupported_cmd
	rec = doRequest(s, "POST", "/channels", `{"action":"create_private","name":"X"}`, nil)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("device rejection: status = %d, want 502", rec.Code)
	}

	// The set action routes through its own error field (SetChannel returns
	// only an error, unlike the entry methods that return a channel).
	fc.addChannelErr = nil
	fc.setChannelErr = &meshcore.DeviceError{Code: 0x02} // index out of range
	rec = doRequest(s, "POST", "/channels", `{"action":"set","index":2,"name":"Moved","psk":"`+testPSKHex+`"}`, nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("set device rejection: status = %d, want 404", rec.Code)
	}
}

func TestHandleChannelAddValidation(t *testing.T) {
	s := newTestServer(&fakeClient{})
	cases := []struct{ name, body string }{
		{"invalid JSON", "{nope"},
		{"unknown action", `{"action":"join"}`},
		{"create_private without name", `{"action":"create_private"}`},
		{"create_private with psk", `{"action":"create_private","name":"X","psk":"` + testPSKHex + `"}`},
		{"add without name", `{"action":"add","psk":"` + testPSKHex + `"}`},
		{"add without psk", `{"action":"add","name":"X"}`},
		{"add with index", `{"action":"add","name":"X","psk":"` + testPSKHex + `","index":2}`},
		{"add with short psk", `{"action":"add","name":"X","psk":"aabb"}`},
		{"add with non-hex psk", `{"action":"add","name":"X","psk":"` + strings.Repeat("zz", 16) + `"}`},
		{"set without index", `{"action":"set","name":"X","psk":"` + testPSKHex + `"}`},
		{"set with negative index", `{"action":"set","index":-1,"name":"X","psk":"` + testPSKHex + `"}`},
		{"set with index beyond the byte range", `{"action":"set","index":256,"name":"X","psk":"` + testPSKHex + `"}`},
		{"set without name", `{"action":"set","index":2,"psk":"` + testPSKHex + `"}`},
		{"set without psk", `{"action":"set","index":2,"name":"X"}`},
		{"join_public with psk", `{"action":"join_public","psk":"` + testPSKHex + `"}`},
		{"join_hashtag without name", `{"action":"join_hashtag"}`},
		{"join_hashtag with psk", `{"action":"join_hashtag","name":"#test","psk":"` + testPSKHex + `"}`},
		{"overlong channel name", `{"action":"add","name":"` + strings.Repeat("x", 32) + `","psk":"` + testPSKHex + `"}`},
		{"overlong join_public alias", `{"action":"join_public","name":"` + strings.Repeat("x", 40) + `"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doRequest(s, "POST", "/channels", tc.body, nil)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("body %q: status = %d, want 400", tc.body, rec.Code)
			}
		})
	}
}

func TestHandleChannelDelete(t *testing.T) {
	fc := &fakeClient{}
	s := newTestServer(fc)

	rec := doRequest(s, "DELETE", "/channels/3", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: status = %d (body %s)", rec.Code, rec.Body.String())
	}
	if len(fc.removedChannels) != 1 || fc.removedChannels[0] != 3 {
		t.Errorf("remove not forwarded: %v", fc.removedChannels)
	}
	if body := decodeBody(t, rec); body["status"] != "removed" || body["index"] != float64(3) {
		t.Errorf("delete response: %v", body)
	}

	rec = doRequest(s, "DELETE", "/channels/x", "", nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("non-integer index: status = %d, want 400", rec.Code)
	}
	rec = doRequest(s, "DELETE", "/channels/-1", "", nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("negative index: status = %d, want 400", rec.Code)
	}
	// 256 would be truncated to slot 0 by the library's byte narrowing — it
	// must be refused client-side, not sent to the device.
	rec = doRequest(s, "DELETE", "/channels/256", "", nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("index beyond the byte range: status = %d, want 400", rec.Code)
	}
	if len(fc.removedChannels) != 1 {
		t.Errorf("out-of-range index reached the device: %v", fc.removedChannels)
	}

	fc.removeChannelErr = fmt.Errorf("admin: %w", meshcore.ErrNoResponse)
	rec = doRequest(s, "DELETE", "/channels/3", "", nil)
	if rec.Code != http.StatusGatewayTimeout {
		t.Errorf("unanswered: status = %d, want 504", rec.Code)
	}

	fc.removeChannelErr = &meshcore.DeviceError{Code: 0x02} // invalid slot index
	rec = doRequest(s, "DELETE", "/channels/99", "", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("device not_found: status = %d, want 404", rec.Code)
	}
}

// TestHandleAdminWaitsForInFlightSend pins the serialization contract for the
// admin endpoints: the driver matches device replies by frame code (the wire
// has no sequence numbers), so an admin round trip issued while a send is in
// flight must queue behind it instead of interleaving frames on the wire —
// same rule /cli already follows.
func TestHandleAdminWaitsForInFlightSend(t *testing.T) {
	yorick := &meshcore.Contact{Name: "Yorick 💀", PublicKey: []byte{1, 2, 3, 4}}
	fc := &fakeClient{
		contacts:        map[string]*meshcore.Contact{"yorick": yorick},
		listChannelsRes: []*meshcore.Channel{{Index: 1, Name: "Team", PSK: make([]byte, 16)}},
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
	fc.adminHook = func() {
		mu.Lock()
		events = append(events, "admin")
		mu.Unlock()
	}

	sendDone := make(chan struct{})
	go func() {
		doRequest(s, "POST", "/send", `{"type":"dm","to":"Yorick 💀","text":"hi"}`, nil)
		close(sendDone)
	}()
	<-sendStarted

	adminDone := make(chan struct{})
	go func() {
		doRequest(s, "GET", "/channels?full", "", nil)
		close(adminDone)
	}()
	select {
	case <-adminDone:
		t.Fatal("admin listing completed while /send was still in flight")
	case <-time.After(50 * time.Millisecond):
	}

	close(releaseSend)
	<-sendDone
	<-adminDone

	mu.Lock()
	defer mu.Unlock()
	if len(events) != 2 || events[0] != "send" || events[1] != "admin" {
		t.Errorf("device round trips interleaved: events = %v, want [send admin]", events)
	}
}
