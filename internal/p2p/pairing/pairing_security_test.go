package pairing

import "testing"

func TestValidateConfirmRejectsIncomingAndAlreadyPairedBypasses(t *testing.T) {
	m := New()

	// Recording an incoming handshake from an untrusted peer must NOT authorize
	// /approve-confirm without us having initiated a pairing request.
	m.RecordIncoming(IncomingRequest{
		PeerID:     "attacker-1",
		DeviceName: "Attacker",
		Address:    "10.0.0.99",
		Port:       8383,
	})
	if m.ValidateConfirm("attacker-1", "10.0.0.99", 8383, false) {
		t.Fatal("ValidateConfirm accepted unsolicited confirm based on incoming handshake")
	}

	// Already-paired peers must not bypass the grace window either (otherwise an
	// unauthenticated sender can hijack/overwrite their address or key).
	if m.ValidateConfirm("victim-peer", "10.0.0.99", 8383, true) {
		t.Fatal("ValidateConfirm accepted unsolicited confirm based on alreadyPaired")
	}

	// Large/negative ports must be rejected safely without panicking.
	if m.ValidateConfirm("peer-a", "10.0.0.2", 100_000_000, false) {
		t.Fatal("ValidateConfirm accepted out-of-range port")
	}
	if m.ValidateConfirm("peer-a", "10.0.0.2", -5, false) {
		t.Fatal("ValidateConfirm accepted negative port")
	}

	// A legitimately initiated pairing is accepted once and consumed on replay.
	m.RecordSent("peer-ok", "10.0.0.5", "10.0.0.5:8383")
	if !m.ValidateConfirm("peer-ok", "10.0.0.5", 8383, false) {
		t.Fatal("ValidateConfirm rejected valid initiated pairing")
	}
	if m.ValidateConfirm("peer-ok", "10.0.0.5", 8383, false) {
		t.Fatal("ValidateConfirm allowed replay of consumed pairing")
	}
}
