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
)

// Options carries the gateway's policy settings.
type Options struct {
	// Token enables bearer auth when non-empty: every request must carry
	// "Authorization: Bearer <token>".
	Token string
	// ChannelTransmissions is the max number of transmissions per channel
	// message (deduplicated by receivers). 0 defaults to 3.
	ChannelTransmissions int
}

// Server serves the HTTP gateway API on top of a live meshcore client.
type Server struct {
	client *meshcore.MeshCoreClient
	inbox  *companion.Inbox
	opts   Options
	log    *slog.Logger
}

// Routes builds the mux with the API endpoints, wrapped in auth.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /get", s.handleGet)
	mux.HandleFunc("GET /messages", s.handleMessages)
	mux.HandleFunc("GET /contacts", s.handleContacts)
	mux.HandleFunc("GET /channels", s.handleChannels)
	mux.HandleFunc("POST /send", s.handleSend)
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
	msgs := s.inbox.Since(after)
	writeJSON(w, http.StatusOK, struct {
		Messages []companion.Message `json:"messages"`
		LastID   int64               `json:"last_id"`
	}{Messages: msgs, LastID: s.inbox.LastID()})
}

// contactInfo is a contact as exposed by GET /contacts.
type contactInfo struct {
	Name     string    `json:"name"`
	Key      string    `json:"key"` // full hex public key
	LastSeen time.Time `json:"last_seen"`
}

// handleContacts lists known contacts, sorted by name — a directory for
// clients resolving recipients before POST /send.
func (s *Server) handleContacts(w http.ResponseWriter, _ *http.Request) {
	contacts := s.client.GetContacts()
	list := make([]contactInfo, 0, len(contacts))
	for _, c := range contacts {
		list = append(list, contactInfo{Name: c.Name, Key: fmt.Sprintf("%x", c.PublicKey), LastSeen: c.LastSeen})
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
func (s *Server) handleChannels(w http.ResponseWriter, _ *http.Request) {
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

// sendRequest is the POST /send body.
type sendRequest struct {
	Type string `json:"type"` // "dm" | "channel"
	To   string `json:"to"`   // contact name or hex pubkey prefix (dm); channel name or index (channel)
	Text string `json:"text"`
	Ack  bool   `json:"ack"` // true = wait for delivery confirmation instead of sent
}

func (s *Server) handleSend(w http.ResponseWriter, r *http.Request) {
	var req sendRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&req); err != nil {
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
	err := s.client.SendMessageWithRetry(contact, req.Text, req.Ack, savedPathTries, resetPathTries, sendTimeoutPerTry)
	s.writeSendResult(w, err)
}

func (s *Server) sendChannel(w http.ResponseWriter, req sendRequest) {
	ch := s.resolveChannel(req.To)
	if ch == nil {
		writeError(w, http.StatusNotFound, "channel not found: "+req.To)
		return
	}
	transmissions := s.opts.ChannelTransmissions
	if transmissions <= 0 {
		transmissions = 3
	}
	s.log.Info("POST /send channel", "to", ch.Name, "text", req.Text, "ack", req.Ack)
	err := s.client.SendChannelMessageWithRetry(ch, req.Text, req.Ack, channelMaxRetries, transmissions, sendTimeoutPerTry)
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

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	// Past WriteHeader a failure can only truncate the response; nothing
	// actionable remains, so the encode error is dropped.
	_ = json.NewEncoder(w).Encode(v)
}
