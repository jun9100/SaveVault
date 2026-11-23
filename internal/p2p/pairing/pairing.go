// Package pairing implements the P2P pairing handshake state:
// incoming requests awaiting user approval, and a 2-minute grace window
// guarding /approve-confirm against unsolicited spoofing — ported from the
// handshake logic in src/daemon/p2p/routes.js.
package pairing

import (
	"strconv"
	"sync"
	"time"
)

// graceWindow is how long an initiated handshake stays valid for the
// counterpart's approve-confirm.
const graceWindow = 2 * time.Minute

// IncomingRequest is a handshake from another device awaiting the local
// user's approval.
type IncomingRequest struct {
	PeerID     string `json:"peerId"`
	DeviceName string `json:"deviceName"`
	DeviceType string `json:"deviceType"`
	Address    string `json:"address"`
	Port       int    `json:"port"`
	ReceivedAt int64  `json:"receivedAt"` // unix ms
	IsWan      bool   `json:"isWan"`
	// PublicKey is the asking device's X25519 public half, held until the
	// user approves and it can be pinned. Empty from a build without
	// end-to-end encryption, which pairs exactly as before.
	PublicKey string `json:"-"`
}

// Manager is safe for concurrent use.
type Manager struct {
	mu       sync.Mutex
	incoming map[string]IncomingRequest // keyed by peerId
	sent     map[string]time.Time       // keys: peerId, ip, "ip:port"
	now      func() time.Time
}

// New creates a Manager.
func New() *Manager {
	return &Manager{
		incoming: map[string]IncomingRequest{},
		sent:     map[string]time.Time{},
		now:      time.Now,
	}
}

// RecordIncoming stores (or refreshes) a handshake awaiting approval.
func (m *Manager) RecordIncoming(req IncomingRequest) {
	m.mu.Lock()
	defer m.mu.Unlock()
	req.ReceivedAt = m.now().UnixMilli()
	m.incoming[req.PeerID] = req
}

// TakeIncoming removes and returns a pending request (approval or
// rejection consumes it).
func (m *Manager) TakeIncoming(peerID string) (IncomingRequest, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	req, ok := m.incoming[peerID]
	if ok {
		delete(m.incoming, peerID)
	}
	return req, ok
}

// HasIncoming reports whether a handshake from this peer is pending.
func (m *Manager) HasIncoming(peerID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.incoming[peerID]
	return ok
}

// PendingRequests lists handshakes awaiting user approval.
func (m *Manager) PendingRequests() []IncomingRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]IncomingRequest, 0, len(m.incoming))
	for _, req := range m.incoming {
		out = append(out, req)
	}
	return out
}

// RecordSent marks that we initiated pairing toward the given keys
// (peer id and/or address forms), opening the approve-confirm grace
// window.
func (m *Manager) RecordSent(keys ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	for _, k := range keys {
		if k != "" {
			m.sent[k] = now
		}
	}
}

// ValidateConfirm decides whether an /approve-confirm may be accepted:
// we must have initiated a handshake to this peer/address within the grace
// window. Valid confirms consume the matching sent entries to prevent replay.
//
// Unauthenticated incoming handshakes and existing paired records are NOT
// sufficient on their own: otherwise any LAN/room attacker could pair without
// user approval (by sending /handshake followed by /approve-confirm) or
// overwrite an existing paired peer's pinned key and address.
func (m *Manager) ValidateConfirm(peerID, clientIP string, port int, alreadyPaired bool) bool {
	if port < 0 || port > 65535 {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.now()
	fresh := func(key string) bool {
		if key == "" {
			return false
		}
		t, ok := m.sent[key]
		return ok && now.Sub(t) < graceWindow
	}

	keys := candidateKeys(peerID, clientIP, port)
	for _, k := range keys {
		if fresh(k) {
			// Consume every candidate entry to prevent replay.
			for _, kk := range keys {
				delete(m.sent, kk)
			}
			return true
		}
	}
	return false
}

// candidateKeys covers peer-id, plain-ip, ip:port, and the
// localhost/127.0.0.1 alias cross-checks from the JS implementation.
func candidateKeys(peerID, clientIP string, port int) []string {
	keys := make([]string, 0, 5)
	if peerID != "" {
		keys = append(keys, peerID)
	}
	if clientIP != "" {
		keys = append(keys, clientIP)
		if hp := hostPort(clientIP, port); hp != clientIP {
			keys = append(keys, hp)
		}
		switch clientIP {
		case "localhost":
			keys = append(keys, "127.0.0.1", hostPort("127.0.0.1", port))
		case "127.0.0.1", "::1":
			keys = append(keys, "localhost", hostPort("localhost", port))
		}
	}
	return keys
}

func hostPort(host string, port int) string {
	if port <= 0 || port > 65535 {
		return host
	}
	return host + ":" + strconv.Itoa(port)
}
