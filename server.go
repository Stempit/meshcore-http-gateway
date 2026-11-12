package main

import (
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/stempit/meshcore-go"
	"github.com/stempit/meshcore-kit/companion"
)

// Send pacing mirrors the bot dispatcher's parameters: the same retry counts
// and per-attempt confirmation timeouts the radio link is known to tolerate.
const (
	sendTimeoutPerTry = 3 * time.Second
	savedPathTries    = 3
	resetPathTries    = 3
	channelMaxRetries = 1
	// maxCLICommandLen mirrors the firmware's CLI line buffer that meshcore-go
	// enforces client-side (maxCliCommandLen): rejecting over-long commands
	// here turns what the driver would report as a device failure into a 400.
	maxCLICommandLen = 160
	// maxBodyBytes caps every request body; the largest legitimate payload is
	// a /send text, so anything past this is garbage by definition.
	maxBodyBytes = 64 * 1024
)

// Options carries the gateway's policy settings.
type Options struct {
	// Token enables bearer auth when non-empty: every request must carry
	// "Authorization: Bearer <token>".
	Token string
	// ChannelTransmissions is the max number of transmissions per channel
	// message (deduplicated by receivers); loadConfig normalizes unset to 3.
	ChannelTransmissions int
	// Transport is the configured connection type ("bt", "serial" or "tcp"),
	// reported verbatim by GET /status.
	Transport string
}

// deviceClient is the slice of the meshcore client the gateway API uses.
// Server depends on this interface rather than the concrete client, so the
// handlers are testable without a radio.
type deviceClient interface {
	GetContacts() map[string]*meshcore.Contact
	GetChannels() map[int]*meshcore.Channel
	FindContactByName(name string) *meshcore.Contact
	FindContactByKeyPrefix(prefix []byte) *meshcore.Contact
	GetChannelByName(name string) *meshcore.Channel
	GetSelfInfo() *meshcore.SelfInfo
	GetStats() *meshcore.CompanionRadioStats
	IsConnected() bool
	SendMessageWithRetry(contact *meshcore.Contact, text string, waitForDelivery bool, savedPathTries, resetPathTries int, timeout time.Duration) error
	SendChannelMessageWithRetry(channel *meshcore.Channel, text string, waitForDelivery bool, maxRetries, transmissions int, timeout time.Duration) error
	RunCLICommand(cmd string) (string, error)
	// Contact administration (meshcore-go v0.4.0): every call is a
	// synchronous device round trip that updates the driver's cache on
	// success, so the cached directory and /send resolution stay consistent
	// without any gateway-side invalidation.
	GetContactByKey(pubKey []byte) (*meshcore.Contact, error)
	AddContact(contact *meshcore.Contact) error
	RemoveContact(contact *meshcore.Contact) error
	ListContacts() ([]*meshcore.Contact, error)
	// Channel administration: entering a channel is writing a slot.
	ListChannels() ([]*meshcore.Channel, error)
	AddChannel(name string, psk []byte) (*meshcore.Channel, error)
	CreatePrivateChannel(name string) (*meshcore.Channel, error)
	JoinPublicChannel(name string) (*meshcore.Channel, error)
	JoinHashtagChannel(name string) (*meshcore.Channel, error)
	SetChannel(channelIndex int, name string, psk []byte) error
	RemoveChannel(index int) error
}

// Server serves the HTTP gateway API on top of a live meshcore client.
type Server struct {
	client deviceClient
	inbox  *companion.Inbox
	opts   Options
	log    *slog.Logger

	// deviceMu serializes device round trips: message sends and CLI commands
	// must never overlap on the wire. The driver's confirmation slots are
	// command-agnostic (the protocol has no sequence numbers) — a stray OK
	// from a CLI command issued while a send waits can complete the send's
	// confirmation spuriously, and vice versa.
	deviceMu sync.Mutex

	// startedAt is when the HTTP server was built — after the device connect
	// and the startup sync, not process start; GET /status reports it.
	startedAt time.Time
	// clock is the device RTC sampled once at startup (sampleDeviceTime);
	// zero times mean the sample failed and /status omits it.
	clock startupClock
}

// startupClock is the device RTC as sampled once at startup: the reading and
// when it was taken. GET /status serves both so clients can judge the
// device's clock drift against their own.
type startupClock struct {
	DeviceTime time.Time
	SampledAt  time.Time
}

// Routes builds the mux with the API endpoints, wrapped in auth.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /get", s.handleGet)
	mux.HandleFunc("GET /messages", s.handleMessages)
	mux.HandleFunc("GET /contacts", s.handleContacts)
	mux.HandleFunc("GET /contacts/{key}", s.handleContactGet)
	mux.HandleFunc("POST /contacts", s.handleContactAdd)
	mux.HandleFunc("DELETE /contacts/{key}", s.handleContactDelete)
	mux.HandleFunc("GET /channels", s.handleChannels)
	mux.HandleFunc("POST /channels", s.handleChannelAdd)
	mux.HandleFunc("DELETE /channels/{index}", s.handleChannelDelete)
	mux.HandleFunc("GET /status", s.handleStatus)
	mux.HandleFunc("POST /send", s.handleSend)
	mux.HandleFunc("POST /cli", s.handleCLI)
	return s.withAuth(mux)
}

// withAuth enforces the bearer token when one is configured.
func (s *Server) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.opts.Token == "" {
			next.ServeHTTP(w, r)
			return
		}
		const prefix = "Bearer "
		auth := r.Header.Get("Authorization")
		rest := strings.TrimPrefix(auth, prefix)
		// ConstantTimeCompare leaks only the token length, which is not secret
		// material worth protecting; it prevents content probing.
		if !strings.HasPrefix(auth, prefix) || subtle.ConstantTimeCompare([]byte(rest), []byte(s.opts.Token)) != 1 {
			writeError(w, http.StatusUnauthorized, "missing or invalid bearer token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// handleGet drains the unread inbox. Reading marks messages read: the next
// call returns only what arrived after this one.
func (s *Server) handleGet(w http.ResponseWriter, _ *http.Request) {
	msgs := s.inbox.Drain()
	s.log.Info("GET /get", "messages", len(msgs))
	writeJSON(w, http.StatusOK, struct {
		Messages []companion.Message `json:"messages"`
	}{Messages: msgs})
}

// handleMessages returns messages newer than ?after=<id> WITHOUT draining —
// the cursor-based, non-destructive alternative to /get, for clients that
// share the inbox with other consumers. last_id is the newest assigned ID and
// is always safe to use as the next cursor.
func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	after := int64(0)
	if v := r.URL.Query().Get("after"); v != "" {
		var err error
		if after, err = strconv.ParseInt(v, 10, 64); err != nil {
			writeError(w, http.StatusBadRequest, `"after" must be an integer`)
			return
		}
	}
	// One lock: a separate Since + LastID could straddle a new arrival and
	// hand out a cursor that skips it — the kit's "last_id is always safe"
	// contract only holds when the pair is read atomically.
	msgs, lastID := s.inbox.SinceWithLastID(after)
	writeJSON(w, http.StatusOK, struct {
		Messages []companion.Message `json:"messages"`
		LastID   int64               `json:"last_id"`
	}{Messages: msgs, LastID: lastID})
}

// contactInfo is a contact as exposed by GET /contacts.
type contactInfo struct {
	Name     string    `json:"name"`
	Key      string    `json:"key"` // full hex public key
	LastSeen time.Time `json:"last_seen"`
}

// handleContacts lists known contacts, sorted by name — a directory for
// clients resolving recipients before POST /send. With ?full it serves the
// admin view instead: a fresh listing straight from the device with all
// stored fields (listContactsFull) — a device round trip, not a cache read.
func (s *Server) handleContacts(w http.ResponseWriter, r *http.Request) {
	if wantsFullListing(r) {
		s.listContactsFull(w)
		return
	}
	contacts := s.client.GetContacts()
	list := make([]contactInfo, 0, len(contacts))
	for _, c := range contacts {
		list = append(list, contactInfo{Name: c.Name, Key: hex.EncodeToString(c.PublicKey), LastSeen: c.LastSeen})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	writeJSON(w, http.StatusOK, struct {
		Contacts []contactInfo `json:"contacts"`
	}{Contacts: list})
}

// channelInfo is a channel as exposed by GET /channels.
type channelInfo struct {
	Index int    `json:"index"`
	Name  string `json:"name"`
}

// handleChannels lists the configured (non-empty) channels, sorted by index.
// With ?full it serves the admin view instead: every slot including empty
// ones, with key material (listChannelsFull) — a device round trip.
func (s *Server) handleChannels(w http.ResponseWriter, r *http.Request) {
	if wantsFullListing(r) {
		s.listChannelsFull(w)
		return
	}
	channels := s.client.GetChannels()
	list := make([]channelInfo, 0, len(channels))
	for _, ch := range channels {
		if ch.IsEmpty {
			continue
		}
		list = append(list, channelInfo{Index: ch.Index, Name: ch.Name})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Index < list[j].Index })
	writeJSON(w, http.StatusOK, struct {
		Channels []channelInfo `json:"channels"`
	}{Channels: list})
}

// statusDevice is the device half of GET /status: identity and radio config
// from the connect handshake (SelfInfo) — no device traffic at request time.
type statusDevice struct {
	Name       string `json:"name"`
	Model      string `json:"model,omitempty"`
	Firmware   string `json:"firmware,omitempty"`
	FreqHz     uint32 `json:"freq_hz"`
	BwHz       uint32 `json:"bw_hz"`
	Sf         int    `json:"sf"`
	Cr         int    `json:"cr"`
	TxPower    int8   `json:"tx_power"`
	MaxTxPower int8   `json:"max_tx_power"`
	// omitempty: an unset advert location parses as 0,0 — treated as "no
	// location" rather than a node anchored in the Gulf of Guinea.
	Latitude  float64 `json:"lat,omitempty"`
	Longitude float64 `json:"lon,omitempty"`
}

// statusStats is the radio statistics half of GET /status, refreshed by the
// runtime's 30-second keepalive polls; age_secs says how stale a reading is.
type statusStats struct {
	BatteryMv         uint16  `json:"battery_mv"`
	UptimeSecs        uint32  `json:"uptime_secs"`
	Errors            uint16  `json:"errors"`
	QueueLen          uint8   `json:"queue_len"`
	TxAirSecs         uint32  `json:"tx_air_secs"`
	RxAirSecs         uint32  `json:"rx_air_secs"`
	NoiseFloorDbm     int8    `json:"noise_floor_dbm"`
	LastRssiDbm       int8    `json:"last_rssi_dbm"`
	LastSnrDb         float32 `json:"last_snr_db"`
	PacketsRecv       uint32  `json:"packets_recv"`
	PacketsSent       uint32  `json:"packets_sent"`
	PacketsRecvErrors uint32  `json:"packets_recv_errors"`
	AgeSecs           int64   `json:"age_secs"`
}

// statusInfo is the GET /status body: a cached snapshot of the link and the
// device. Everything is read from state the gateway already holds, so a
// /status call never queues behind — or collides with — a send.
type statusInfo struct {
	Connected           bool          `json:"connected"`
	Transport           string        `json:"transport"`
	Device              *statusDevice `json:"device,omitempty"`
	Stats               *statusStats  `json:"stats,omitempty"`
	DeviceTime          *time.Time    `json:"device_time,omitempty"`
	DeviceTimeSampledAt *time.Time    `json:"device_time_sampled_at,omitempty"`
	StartedAt           time.Time     `json:"started_at"`
}

// handleStatus serves the cached device snapshot: connection state, the
// handshake's self info, the keepalive-refreshed stats and the device clock
// sampled at startup. `stats` is omitted until the first keepalive poll lands
// (≤30s after start); `device_time` is omitted when the startup sample failed.
func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	info := statusInfo{
		Connected: s.client.IsConnected(),
		Transport: s.opts.Transport,
		StartedAt: s.startedAt,
	}
	if si := s.client.GetSelfInfo(); si != nil {
		info.Device = &statusDevice{
			Name:       si.Name,
			Model:      si.Model,
			Firmware:   si.FirmwareVersion,
			FreqHz:     si.FreqHz,
			BwHz:       si.BwHz,
			Sf:         si.Sf,
			Cr:         si.Cr,
			TxPower:    si.TxPower,
			MaxTxPower: si.MaxTxPower,
			Latitude:   si.Latitude,
			Longitude:  si.Longitude,
		}
	}
	if st := s.client.GetStats(); st != nil {
		// A clock jump between sample and report could make the age negative;
		// never report one.
		age := max(time.Since(st.LastUpdate), 0)
		info.Stats = &statusStats{
			BatteryMv:         st.BatteryMv,
			UptimeSecs:        st.UptimeSecs,
			Errors:            st.Errors,
			QueueLen:          st.QueueLen,
			TxAirSecs:         st.TxAirSecs,
			RxAirSecs:         st.RxAirSecs,
			NoiseFloorDbm:     st.NoiseFloorDbm,
			LastRssiDbm:       st.LastRssi,
			LastSnrDb:         st.LastSnrDb,
			PacketsRecv:       st.PacketsRecv,
			PacketsSent:       st.PacketsSent,
			PacketsRecvErrors: st.PacketsRecvErrors,
			AgeSecs:           int64(age.Seconds()),
		}
	}
	if !s.clock.DeviceTime.IsZero() {
		ts := s.clock.DeviceTime
		info.DeviceTime = &ts
		at := s.clock.SampledAt
		info.DeviceTimeSampledAt = &at
	}
	writeJSON(w, http.StatusOK, info)
}

// sendRequest is the POST /send body.
type sendRequest struct {
	Type string `json:"type"` // "dm" | "channel"
	To   string `json:"to"`   // contact name or hex pubkey prefix (dm); channel name or index (channel)
	Text string `json:"text"`
	Ack  bool   `json:"ack"` // true = wait for delivery confirmation instead of sent
}

func (s *Server) handleSend(w http.ResponseWriter, r *http.Request) {
	var req sendRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	req.Type = strings.ToLower(strings.TrimSpace(req.Type))
	req.To = strings.TrimSpace(req.To)
	if req.To == "" || strings.TrimSpace(req.Text) == "" {
		writeError(w, http.StatusBadRequest, `"to" and "text" are required`)
		return
	}

	switch req.Type {
	case "dm":
		s.sendDM(w, req)
	case "channel":
		s.sendChannel(w, req)
	default:
		writeError(w, http.StatusBadRequest, `"type" must be "dm" or "channel"`)
	}
}

// resolveContact finds a contact by exact name, or by hex public-key prefix
// when the name lookup misses (the apps identify nodes by hex key prefixes).
// Deliberately strict: the HTTP API addresses recipients exactly as they are
// stored — the forgiving ("yorick" → "Yorick 💀") matching is a voice-only
// convenience in meshcore-mcp-gateway.
func (s *Server) resolveContact(to string) *meshcore.Contact {
	if c := s.client.FindContactByName(to); c != nil {
		return c
	}
	if key, err := hex.DecodeString(strings.ToLower(to)); err == nil && len(key) > 0 {
		return s.client.FindContactByKeyPrefix(key)
	}
	return nil
}

// resolveChannel finds a channel by exact name or numeric index.
func (s *Server) resolveChannel(to string) *meshcore.Channel {
	if ch := s.client.GetChannelByName(to); ch != nil {
		return ch
	}
	if idx, err := strconv.Atoi(to); err == nil {
		if ch, ok := s.client.GetChannels()[idx]; ok && !ch.IsEmpty {
			return ch
		}
	}
	return nil
}

func (s *Server) sendDM(w http.ResponseWriter, req sendRequest) {
	contact := s.resolveContact(req.To)
	if contact == nil {
		writeError(w, http.StatusNotFound, "contact not found: "+req.To)
		return
	}
	s.log.Info("POST /send dm", "to", contact.Name, "text", req.Text, "ack", req.Ack)
	s.deviceMu.Lock()
	defer s.deviceMu.Unlock()
	err := s.client.SendMessageWithRetry(contact, req.Text, req.Ack, savedPathTries, resetPathTries, sendTimeoutPerTry)
	s.writeSendResult(w, err)
}

func (s *Server) sendChannel(w http.ResponseWriter, req sendRequest) {
	ch := s.resolveChannel(req.To)
	if ch == nil {
		writeError(w, http.StatusNotFound, "channel not found: "+req.To)
		return
	}
	s.log.Info("POST /send channel", "to", ch.Name, "text", req.Text, "ack", req.Ack)
	s.deviceMu.Lock()
	defer s.deviceMu.Unlock()
	err := s.client.SendChannelMessageWithRetry(ch, req.Text, req.Ack, channelMaxRetries, s.opts.ChannelTransmissions, sendTimeoutPerTry)
	s.writeSendResult(w, err)
}

// writeSendResult maps a client send error onto the API's status codes:
// success → 200, no confirmation → 504 (may still have been delivered),
// everything else → 502.
func (s *Server) writeSendResult(w http.ResponseWriter, err error) {
	if err == nil {
		writeJSON(w, http.StatusOK, map[string]string{"status": "sent"})
		return
	}
	switch {
	case errors.Is(err, meshcore.ErrNoConfirmation):
		writeError(w, http.StatusGatewayTimeout, err.Error())
	case errors.Is(err, meshcore.ErrDeviceRejected):
		writeError(w, http.StatusBadGateway, err.Error())
	default:
		writeError(w, http.StatusBadGateway, "send failed: "+err.Error())
	}
}

// cliRequest is the POST /cli body.
type cliRequest struct {
	Command string `json:"command"`
}

// handleCLI runs one ASCII CLI command on the device itself (companion
// firmware v14+) and returns the device's verbatim reply — `ver`, `get tx`,
// `set radio ...`, `reboot`, everything in the firmware's docs/cli_commands.md.
//
// The reply is the device's own text, not a parsed result: `get` commands
// answer "> <value>", a failed command answers "Error, ..." — those come back
// as 200 replies (the command ran; the device just said no). HTTP errors are
// for the request itself or the command not running at all: 400 for a
// malformed request, 502 when the device rejects the command (e.g.
// unsupported_cmd on firmware older than v14) or the transport fails, 504
// when the device does not answer in time.
//
// The device path is shared with /send, so the gateway serializes both behind
// deviceMu: a /cli issued while a send is in flight waits for it (tens of
// seconds worst case), and a /send issued while a CLI command waits for its
// reply does the same.
func (s *Server) handleCLI(w http.ResponseWriter, r *http.Request) {
	var req cliRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	cmd := strings.TrimSpace(req.Command)
	if cmd == "" {
		writeError(w, http.StatusBadRequest, `"command" is required`)
		return
	}
	// The 2-character floor mirrors the driver's RunCLICommand minimum: below
	// it the driver rejects without touching the device, so it is client
	// input and gets a 400.
	if len(cmd) < 2 {
		writeError(w, http.StatusBadRequest, "command too short (min 2 characters)")
		return
	}
	if len(cmd) > maxCLICommandLen {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("command too long (%d bytes, max %d)", len(cmd), maxCLICommandLen))
		return
	}
	s.log.Info("POST /cli", "command", cmd)

	s.deviceMu.Lock()
	defer s.deviceMu.Unlock()
	reply, err := s.client.RunCLICommand(cmd)
	if err == nil {
		writeJSON(w, http.StatusOK, map[string]string{"reply": reply})
		return
	}
	var devErr *meshcore.DeviceError
	switch {
	case errors.As(err, &devErr):
		writeError(w, http.StatusBadGateway, err.Error())
	case errors.Is(err, meshcore.ErrNoResponse):
		writeError(w, http.StatusGatewayTimeout, err.Error())
	default:
		writeError(w, http.StatusBadGateway, "cli command failed: "+err.Error())
	}
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	// HTML escaping is off: consumers parse JSON, where \u003c/\u003e are
	// identical to </> anyway, while raw readers (curl, logs) should see the
	// real characters. The response is application/json, never embedded into
	// HTML, so the escaping protects nothing here.
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	// Past WriteHeader a failure can only truncate the response; nothing
	// actionable remains, so the encode error is dropped.
	_ = enc.Encode(v)
}
