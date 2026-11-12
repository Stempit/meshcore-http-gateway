package main

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/stempit/meshcore-go"
)

// Device error codes the admin endpoints map onto richer HTTP statuses than
// the blanket 502. The values are the firmware's ERR_CODE_* (MyMesh.h, stable
// across firmware); meshcore-go renders them in DeviceError.Error() but does
// not export the constants yet.
const (
	devErrNotFound  = 0x02
	devErrTableFull = 0x03
)

// maxNameBytes is the on-device name field: 32 bytes including the
// terminator, so 31 usable bytes. meshcore-go silently truncates past that;
// the gateway rejects instead, so a client never sees a name it did not send.
const maxNameBytes = 31

// maxChannelIndex is the largest slot index the protocol can express: the
// library narrows the slot to one byte (byte(channelIndex) in
// buildSetChannelFrame), so an index of 256 would silently write slot 0 —
// clobbering, say, the default Public channel — instead of failing.
const maxChannelIndex = 255

// writeAdminError maps an error from a synchronous device admin call onto the
// API's status codes and reports whether it wrote a response: not_found →
// 404, table_full / no free slot → 409, unanswered command → 504, every other
// device rejection or transport failure → 502. The error's own wording is
// passed through verbatim — it names the firmware error (DeviceError) or the
// transport failure.
func (s *Server) writeAdminError(w http.ResponseWriter, err error) bool {
	if err == nil {
		return false
	}
	var devErr *meshcore.DeviceError
	switch {
	case errors.As(err, &devErr):
		switch devErr.Code {
		case devErrNotFound:
			writeError(w, http.StatusNotFound, err.Error())
		case devErrTableFull:
			writeError(w, http.StatusConflict, err.Error())
		default:
			writeError(w, http.StatusBadGateway, err.Error())
		}
	case errors.Is(err, meshcore.ErrNoEmptyChannelSlot):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, meshcore.ErrNoResponse):
		writeError(w, http.StatusGatewayTimeout, err.Error())
	default:
		writeError(w, http.StatusBadGateway, err.Error())
	}
	return true
}

// wantsFullListing reports whether the request asked for the device
// round-trip listing: ?full or ?full=true enable it, ?full=false / ?full=0
// keep the cheap cached listing.
func wantsFullListing(r *http.Request) bool {
	q := r.URL.Query()
	if _, ok := q["full"]; !ok {
		return false
	}
	v := q.Get("full")
	return v != "false" && v != "0"
}

// parseHexKey decodes a 64-hex-character public key — the full 32-byte key
// the device addresses contacts by (a prefix is not enough here, unlike the
// recipient resolution in /send).
func parseHexKey(s string) ([]byte, error) {
	key, err := hex.DecodeString(s)
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("key must be 64 hex characters (32 bytes)")
	}
	return key, nil
}

// parsePSK decodes a channel key. Exactly 16 bytes: the firmware rejects
// 256-bit keys outright and mis-parses shorter frames, so meshcore-go
// enforces the length client-side.
func parsePSK(s string) ([]byte, error) {
	psk, err := hex.DecodeString(s)
	if err != nil || len(psk) != 16 {
		return nil, fmt.Errorf("psk must be 32 hex characters (exactly 16 bytes)")
	}
	return psk, nil
}

// advTypeNames maps the write API's contact type names onto the firmware's
// advert types (AdvertDataHelpers.h).
var advTypeNames = map[string]int{
	"chat":     meshcore.AdvTypeChat,
	"repeater": meshcore.AdvTypeRepeater,
	"room":     meshcore.AdvTypeRoom,
	"sensor":   meshcore.AdvTypeSensor,
}

// contactFlagNames maps the write API's flag names onto the ContactFlag*
// bitmask values.
var contactFlagNames = map[string]int{
	"favorite":  meshcore.ContactFlagFavorite,
	"tele_base": meshcore.ContactFlagTeleBase,
	"tele_loc":  meshcore.ContactFlagTeleLoc,
	"tele_env":  meshcore.ContactFlagTeleEnv,
}

// parseFlags renders the request's flag names as a bitmask.
func parseFlags(names []string) (int, error) {
	mask := 0
	for _, n := range names {
		bit, ok := contactFlagNames[strings.ToLower(strings.TrimSpace(n))]
		if !ok {
			return 0, fmt.Errorf("unknown flag %q (known: favorite, tele_base, tele_loc, tele_env)", n)
		}
		mask |= bit
	}
	return mask, nil
}

// flagNames renders a flags bitmask as the API's flag names, sorted for
// stable output; bits outside the known set are dropped — the write API has
// no names for them, so a client could not reproduce them anyway.
func flagNames(flags int) []string {
	names := make([]string, 0, len(contactFlagNames))
	for name, bit := range contactFlagNames {
		if flags&bit != 0 {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// ============================================================================
// Wire types
// ============================================================================

// contactDetails is a contact with all device-stored fields, served by the
// admin endpoints (GET /contacts?full, GET /contacts/{key}).
type contactDetails struct {
	Name     string   `json:"name"`
	Key      string   `json:"key"` // full hex public key
	TypeName string   `json:"type_name"`
	Type     int      `json:"type"`
	Flags    []string `json:"flags"`
	// PathLength is the firmware-encoded out_path_len byte: hop count (low 6
	// bits) + bytes-per-hop (top 2 bits); 0xFF = flood/unknown.
	PathLength int       `json:"path_length"`
	PathHex    string    `json:"path_hex,omitempty"` // meaningful path bytes only
	Latitude   *float64  `json:"lat,omitempty"`
	Longitude  *float64  `json:"lon,omitempty"`
	LastSeen   time.Time `json:"last_seen"`
	// LastMessageAt is omitted when no message was exchanged yet.
	LastMessageAt *time.Time `json:"last_message_at,omitempty"`
	IsActive      bool       `json:"is_active"`
	LastMod       uint32     `json:"last_mod"` // device RTC timestamp of the last change
}

// newContactDetails renders a library contact for the admin endpoints. The
// contact is a driver-issued deep copy, so reading it needs no lock.
func newContactDetails(ct *meshcore.Contact) contactDetails {
	d := contactDetails{
		Name:       ct.Name,
		Key:        ct.PublicKeyHex(),
		TypeName:   ct.TypeName(),
		Type:       ct.Type,
		Flags:      flagNames(ct.Flags),
		PathLength: ct.PathLength,
		PathHex:    hex.EncodeToString(ct.Path),
		Latitude:   ct.Latitude,
		Longitude:  ct.Longitude,
		LastSeen:   ct.LastSeen,
		IsActive:   ct.IsActive,
		LastMod:    ct.LastMod,
	}
	if !ct.LastMessageAt.IsZero() {
		t := ct.LastMessageAt
		d.LastMessageAt = &t
	}
	return d
}

// channelDetails is a channel slot with its key material, served by the admin
// endpoints. psk_hex is the channel's secret — the same value a QR code
// meshcore://channel/add?name=...&secret=<hex> carries. It is returned
// unconditionally (the bearer token is the gate), so deploy the gateway
// accordingly. hash_byte is how group packets are addressed on air — the way
// to correlate a slot with raw 0x88 frames.
type channelDetails struct {
	Index    int    `json:"index"`
	Name     string `json:"name"`
	PSKHex   string `json:"psk_hex"`
	HashByte int    `json:"hash_byte"`
	IsEmpty  bool   `json:"is_empty"`
}

func newChannelDetails(ch *meshcore.Channel) channelDetails {
	return channelDetails{
		Index:    ch.Index,
		Name:     ch.Name,
		PSKHex:   ch.PSKHex(),
		HashByte: int(ch.HashByte()),
		IsEmpty:  ch.IsEmpty,
	}
}

// contactRequest is the POST /contacts body. The device upserts on the full
// public key, so sending an existing key updates the entry.
type contactRequest struct {
	Key   string   `json:"key"` // 64 hex characters
	Name  string   `json:"name"`
	Type  string   `json:"type"`  // chat (default) | repeater | room | sensor
	Flags []string `json:"flags"` // favorite | tele_base | tele_loc | tele_env
	Lat   *float64 `json:"lat"`
	Lon   *float64 `json:"lon"`
}

// channelRequest is the POST /channels body, dispatched on "action" the way
// /send is dispatched on "type".
type channelRequest struct {
	Action string `json:"action"` // create_private | add | join_public | join_hashtag | set
	Name   string `json:"name"`
	PSK    string `json:"psk"`   // 32 hex characters, required for add/set
	Index  *int   `json:"index"` // required for set
}

// ============================================================================
// Contacts
// ============================================================================

// listContactsFull serves GET /contacts?full: a fresh listing straight from
// the device (CMD_GET_CONTACTS … END_OF_CONTACTS) instead of the cached
// directory. The device iteration is all-radio work, so it queues behind
// deviceMu like every other device round trip.
func (s *Server) listContactsFull(w http.ResponseWriter) {
	s.deviceMu.Lock()
	contacts, err := s.client.ListContacts()
	s.deviceMu.Unlock()
	if s.writeAdminError(w, err) {
		return
	}
	list := make([]contactDetails, 0, len(contacts))
	for _, ct := range contacts {
		list = append(list, newContactDetails(ct))
	}
	writeJSON(w, http.StatusOK, struct {
		Contacts []contactDetails `json:"contacts"`
	}{Contacts: list})
}

// handleContactGet serves GET /contacts/{key}: one contact read back from the
// device (CMD_GET_CONTACT_BY_KEY). Where the cached directory shows what the
// gateway has seen, this round trip shows what the device holds right now —
// the "verify what landed" read.
func (s *Server) handleContactGet(w http.ResponseWriter, r *http.Request) {
	key, err := parseHexKey(r.PathValue("key"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.deviceMu.Lock()
	ct, err := s.client.GetContactByKey(key)
	s.deviceMu.Unlock()
	if s.writeAdminError(w, err) {
		return
	}
	if ct == nil {
		writeError(w, http.StatusNotFound, "contact not found")
		return
	}
	writeJSON(w, http.StatusOK, newContactDetails(ct))
}

// handleContactAdd serves POST /contacts: write one contact into the device's
// storage (CMD_ADD_UPDATE_CONTACT). One call both adds new contacts and
// updates existing ones — the firmware looks the entry up by the full public
// key. The device confirms the write; table_full comes back as 409.
func (s *Server) handleContactAdd(w http.ResponseWriter, r *http.Request) {
	var req contactRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	key, err := parseHexKey(strings.TrimSpace(req.Key))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeError(w, http.StatusBadRequest, `"name" is required`)
		return
	}
	if len(name) > maxNameBytes {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("name too long (%d bytes, max %d)", len(name), maxNameBytes))
		return
	}
	if req.Type == "" {
		req.Type = "chat" // what a manually added contact almost always is
	}
	atype, ok := advTypeNames[strings.ToLower(strings.TrimSpace(req.Type))]
	if !ok {
		writeError(w, http.StatusBadRequest, `"type" must be one of chat, repeater, room, sensor`)
		return
	}
	flags, err := parseFlags(req.Flags)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if (req.Lat == nil) != (req.Lon == nil) {
		writeError(w, http.StatusBadRequest, `"lat" and "lon" must be given together`)
		return
	}
	// The wire encodes coordinates as degrees ×1e6 in int32; anything beyond
	// the geographic ranges overflows device-side into garbage.
	if req.Lat != nil && (*req.Lat < -90 || *req.Lat > 90) {
		writeError(w, http.StatusBadRequest, `"lat" must be between -90 and 90`)
		return
	}
	if req.Lon != nil && (*req.Lon < -180 || *req.Lon > 180) {
		writeError(w, http.StatusBadRequest, `"lon" must be between -180 and 180`)
		return
	}

	ct := &meshcore.Contact{
		PublicKey: key,
		Name:      name,
		Type:      atype,
		Flags:     flags,
		Latitude:  req.Lat,
		Longitude: req.Lon,
		// Path stays empty: a manually added contact starts flood-routed and
		// the device learns a real path from adverts and traffic.
	}
	s.log.Info("POST /contacts", "key", ct.PublicKeyHex(), "name", name, "type", req.Type)
	s.deviceMu.Lock()
	defer s.deviceMu.Unlock()
	if err := s.client.AddContact(ct); err != nil {
		s.writeAdminError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "saved", "key": ct.PublicKeyHex()})
}

// handleContactDelete serves DELETE /contacts/{key}: remove one contact from
// the device's storage (CMD_REMOVE_CONTACT).
func (s *Server) handleContactDelete(w http.ResponseWriter, r *http.Request) {
	key, err := parseHexKey(r.PathValue("key"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.log.Info("DELETE /contacts", "key", hex.EncodeToString(key))
	s.deviceMu.Lock()
	defer s.deviceMu.Unlock()
	if err := s.client.RemoveContact(&meshcore.Contact{PublicKey: key}); err != nil {
		s.writeAdminError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "removed"})
}

// ============================================================================
// Channels
// ============================================================================

// listChannelsFull serves GET /channels?full: every slot including the empty
// ones, with key material — the admin view over the cached, non-empty listing
// (unsynced slots are fetched per slot via CMD_GET_CHANNEL).
func (s *Server) listChannelsFull(w http.ResponseWriter) {
	s.deviceMu.Lock()
	channels, err := s.client.ListChannels()
	s.deviceMu.Unlock()
	if s.writeAdminError(w, err) {
		return
	}
	list := make([]channelDetails, 0, len(channels))
	for _, ch := range channels {
		list = append(list, newChannelDetails(ch))
	}
	writeJSON(w, http.StatusOK, struct {
		Channels []channelDetails `json:"channels"`
	}{Channels: list})
}

// validateChannelRequest checks a POST /channels body against its action and
// returns the decoded PSK plus the 400 message when the request is malformed.
// The PSK is parsed before the action switch, so a bad key is reported even
// when the action is unknown too — same order as the checks themselves.
func validateChannelRequest(req channelRequest) (psk []byte, msg string) {
	if req.PSK != "" {
		var err error
		if psk, err = parsePSK(req.PSK); err != nil {
			return nil, err.Error()
		}
	}
	if len(req.Name) > maxNameBytes {
		return nil, fmt.Sprintf("name too long (%d bytes, max %d)", len(req.Name), maxNameBytes)
	}
	if req.Index != nil && req.Action != "set" {
		return nil, `"index" is only accepted for set`
	}
	switch req.Action {
	case "create_private":
		if req.Name == "" {
			return nil, `"name" is required for create_private`
		}
		if psk != nil {
			return nil, `"psk" is not accepted for create_private (a fresh random key is generated)`
		}
	case "add":
		if req.Name == "" {
			return nil, `"name" is required for add`
		}
		if psk == nil {
			return nil, `"psk" is required for add`
		}
	case "join_public":
		// Name is optional: the library substitutes "Public", the firmware's
		// own alias for the standard channel.
		if psk != nil {
			return nil, `"psk" is not accepted for join_public (the firmware key is fixed)`
		}
	case "join_hashtag":
		if req.Name == "" {
			return nil, `"name" is required for join_hashtag`
		}
		if psk != nil {
			return nil, `"psk" is not accepted for join_hashtag (the key is derived from the name)`
		}
	case "set":
		if req.Index == nil || *req.Index < 0 || *req.Index > maxChannelIndex {
			return nil, fmt.Sprintf(`"index" must be an integer between 0 and %d for set`, maxChannelIndex)
		}
		if req.Name == "" {
			return nil, `"name" is required for set (use DELETE /channels/{index} to remove a channel)`
		}
		if psk == nil {
			return nil, `"psk" is required for set`
		}
	default:
		return nil, `"action" must be one of create_private, add, join_public, join_hashtag, set`
	}
	return psk, ""
}

// handleChannelAdd serves POST /channels: the four ways to enter a channel
// (there is no join command on the wire — entering *is* writing a slot) plus
// the direct slot write for point edits.
func (s *Server) handleChannelAdd(w http.ResponseWriter, r *http.Request) {
	var req channelRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	req.Name = strings.TrimSpace(req.Name)

	// Validate before touching deviceMu: input errors must not queue behind
	// an in-flight send.
	psk, msg := validateChannelRequest(req)
	if msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}

	// Log the index value, not the pointer: slog renders a plain *int as its
	// address.
	var logIndex any
	if req.Index != nil {
		logIndex = *req.Index
	}
	s.log.Info("POST /channels", "action", req.Action, "name", req.Name, "index", logIndex)
	var (
		ch  *meshcore.Channel
		err error
	)
	s.deviceMu.Lock()
	defer s.deviceMu.Unlock()
	switch req.Action {
	case "create_private":
		ch, err = s.client.CreatePrivateChannel(req.Name)
	case "add":
		ch, err = s.client.AddChannel(req.Name, psk)
	case "join_public":
		ch, err = s.client.JoinPublicChannel(req.Name)
	case "join_hashtag":
		ch, err = s.client.JoinHashtagChannel(req.Name)
	case "set":
		err = s.client.SetChannel(*req.Index, req.Name, psk)
		if err == nil {
			ch = &meshcore.Channel{Index: *req.Index, Name: req.Name, PSK: psk}
		}
	}
	if s.writeAdminError(w, err) {
		return
	}
	if ch == nil {
		// Unreachable with the real client (a confirmed write always returns
		// the slot), kept so a misbehaving test double cannot panic the
		// handler on the nil deref below.
		writeError(w, http.StatusBadGateway, "device confirmed the write but returned no channel")
		return
	}
	writeJSON(w, http.StatusOK, newChannelDetails(ch))
}

// handleChannelDelete serves DELETE /channels/{index}: free a slot by
// overwriting it with an empty name and a zero key — the protocol's way of
// deleting. Just clearing the name would leave a named slot with a zero key
// that keeps catching null-key traffic.
func (s *Server) handleChannelDelete(w http.ResponseWriter, r *http.Request) {
	idx, err := strconv.Atoi(r.PathValue("index"))
	// The upper bound matters: the library narrows the slot to one byte, so
	// an unchecked 256 would write slot 0 — deleting the Public channel
	// while reporting success.
	if err != nil || idx < 0 || idx > maxChannelIndex {
		writeError(w, http.StatusBadRequest, fmt.Sprintf(`"index" must be an integer between 0 and %d`, maxChannelIndex))
		return
	}
	s.log.Info("DELETE /channels", "index", idx)
	s.deviceMu.Lock()
	defer s.deviceMu.Unlock()
	if err := s.client.RemoveChannel(idx); err != nil {
		s.writeAdminError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "removed", "index": idx})
}
