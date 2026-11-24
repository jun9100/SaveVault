package mobilecore

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestMobileCoreE2EEKeyAgreementSealOpenAndFingerprint(t *testing.T) {
	idAJSON, err := GenerateIdentityJSON()
	if err != nil {
		t.Fatalf("GenerateIdentityJSON A: %v", err)
	}
	idBJSON, err := GenerateIdentityJSON()
	if err != nil {
		t.Fatalf("GenerateIdentityJSON B: %v", err)
	}
	var idA, idB identityPayload
	if err := json.Unmarshal([]byte(idAJSON), &idA); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(idBJSON), &idB); err != nil {
		t.Fatal(err)
	}

	sharedAB, err := DeriveSharedKeyBase64(idA.PrivateKey, idB.PublicKey)
	if err != nil {
		t.Fatalf("DeriveSharedKey AB: %v", err)
	}
	sharedBA, err := DeriveSharedKeyBase64(idB.PrivateKey, idA.PublicKey)
	if err != nil {
		t.Fatalf("DeriveSharedKey BA: %v", err)
	}
	if sharedAB != sharedBA {
		t.Fatal("shared payload keys did not match between peers")
	}

	authAB, err := DeriveAuthKeyBase64(idA.PrivateKey, idB.PublicKey)
	if err != nil {
		t.Fatalf("DeriveAuthKey AB: %v", err)
	}
	if authAB == sharedAB {
		t.Fatal("auth key must be domain-separated from payload key")
	}

	fpAB, err := PairingFingerprint(idA.PublicKey, idB.PublicKey)
	if err != nil {
		t.Fatalf("PairingFingerprint AB: %v", err)
	}
	fpBA, err := PairingFingerprint(idB.PublicKey, idA.PublicKey)
	if err != nil {
		t.Fatalf("PairingFingerprint BA: %v", err)
	}
	if fpAB != fpBA || fpAB == "" {
		t.Fatalf("fingerprints did not match symmetrically: %q vs %q", fpAB, fpBA)
	}

	plain := []byte("savevault-android-to-pc-payload-block")
	sealed, err := SealPayload(sharedAB, plain)
	if err != nil {
		t.Fatalf("SealPayload: %v", err)
	}
	got, err := OpenPayload(sharedBA, sealed)
	if err != nil {
		t.Fatalf("OpenPayload: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("decrypted mismatch: got=%q want=%q", got, plain)
	}

	// Tampering a byte must fail AEAD authentication.
	sealed[len(sealed)-1] ^= 0x01
	if _, err := OpenPayload(sharedBA, sealed); err == nil {
		t.Fatal("expected tampered payload to fail decryption")
	}
}

func TestMobileCoreManifestBuildDiffAndLanPingCodec(t *testing.T) {
	dirA := t.TempDir()
	dirB := t.TempDir()
	if err := os.WriteFile(filepath.Join(dirA, "slot1.sav"), []byte("level-1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dirB, "slot1.sav"), []byte("level-2"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dirB, "slot2.sav"), []byte("new-slot"), 0o644); err != nil {
		t.Fatal(err)
	}

	mA, err := BuildDirectoryManifestJSON(dirA)
	if err != nil {
		t.Fatalf("BuildDirectoryManifestJSON A: %v", err)
	}
	mB, err := BuildDirectoryManifestJSON(dirB)
	if err != nil {
		t.Fatalf("BuildDirectoryManifestJSON B: %v", err)
	}
	diffJSON, err := DiffManifestsJSON(mA, mB)
	if err != nil {
		t.Fatalf("DiffManifestsJSON: %v", err)
	}
	var diff map[string]any
	if err := json.Unmarshal([]byte(diffJSON), &diff); err != nil {
		t.Fatal(err)
	}
	if len(diff) == 0 {
		t.Fatalf("expected non-empty diff JSON, got %s", diffJSON)
	}

	pingRaw, err := EncodeLanPing("android-odin2", "Odin 2", "android", 8384)
	if err != nil {
		t.Fatalf("EncodeLanPing: %v", err)
	}
	decoded, err := DecodeLanPing(pingRaw)
	if err != nil {
		t.Fatalf("DecodeLanPing: %v", err)
	}
	if decoded == "" {
		t.Fatal("expected non-empty decoded ping")
	}
	if _, err := DecodeLanPing(`{"type":"opensave-ping","nodeId":"deck-1","deviceName":"Steam Deck","deviceType":"linux","port":8384}`); err != nil {
		t.Fatalf("legacy opensave-ping should remain accepted: %v", err)
	}
	if _, err := DecodeLanPing(`{"type":"unknown","nodeId":"x","port":8384}`); err == nil {
		t.Fatal("expected unknown ping type to fail")
	}
}
