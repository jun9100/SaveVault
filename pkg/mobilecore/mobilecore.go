// Package mobilecore exposes a gomobile-compatible bridge over SaveVault's
// X25519 + XChaCha20-Poly1305 E2EE primitives, incremental block delta engine,
// and LAN peer discovery wire protocol so Android/iOS builds can bind this
// package via `gomobile bind -target=android ./pkg/mobilecore` or verify wire
// interoperability against the desktop core.
package mobilecore

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jun9100/savevault/internal/delta"
	"github.com/jun9100/savevault/internal/e2ee"
	"github.com/jun9100/savevault/internal/p2p/discovery"
)

// DefaultLanDiscoveryPort is the fixed UDP discovery port used by SaveVault
// desktop and Steam Deck peers (8385).
const DefaultLanDiscoveryPort = discovery.DefaultPort

type identityPayload struct {
	PrivateKey string `json:"privateKey"`
	PublicKey  string `json:"publicKey"`
}

// GenerateIdentityJSON generates a new X25519 key pair encoded as JSON.
func GenerateIdentityJSON() (string, error) {
	id, err := e2ee.GenerateIdentity()
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(identityPayload{
		PrivateKey: e2ee.EncodeKey(id.Private),
		PublicKey:  e2ee.EncodeKey(id.Public),
	})
	return string(raw), err
}

func derivePairwiseKey(myPrivateKeyBase64, peerPublicKeyBase64 string, fn func([]byte, []byte) ([]byte, error)) (string, error) {
	priv, err := e2ee.DecodeKey(myPrivateKeyBase64)
	if err != nil {
		return "", fmt.Errorf("invalid private key: %w", err)
	}
	pub, err := e2ee.DecodeKey(peerPublicKeyBase64)
	if err != nil {
		return "", fmt.Errorf("invalid peer public key: %w", err)
	}
	key, err := fn(priv, pub)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(key), nil
}

// DeriveSharedKeyBase64 computes the X25519 + HKDF-SHA256 payload encryption
// key shared between this device and a paired peer.
func DeriveSharedKeyBase64(myPrivateKeyBase64, peerPublicKeyBase64 string) (string, error) {
	return derivePairwiseKey(myPrivateKeyBase64, peerPublicKeyBase64, e2ee.SharedKey)
}

// DeriveAuthKeyBase64 computes the domain-separated X25519 + HKDF-SHA256
// request authentication key used to sign LAN/relay requests.
func DeriveAuthKeyBase64(myPrivateKeyBase64, peerPublicKeyBase64 string) (string, error) {
	return derivePairwiseKey(myPrivateKeyBase64, peerPublicKeyBase64, e2ee.AuthKey)
}

// PairingFingerprint returns the symmetric 6x4 hex pairing verification code
// derived from the two peers' X25519 public keys (order-independent).
func PairingFingerprint(publicKeyABase64, publicKeyBBase64 string) (string, error) {
	pubA, err := e2ee.DecodeKey(publicKeyABase64)
	if err != nil {
		return "", fmt.Errorf("invalid public key A: %w", err)
	}
	pubB, err := e2ee.DecodeKey(publicKeyBBase64)
	if err != nil {
		return "", fmt.Errorf("invalid public key B: %w", err)
	}
	return e2ee.Fingerprint(pubA, pubB), nil
}

// SealPayload encrypts plaintext with XChaCha20-Poly1305 using the base64
// shared key derived by DeriveSharedKeyBase64.
func SealPayload(sharedKeyBase64 string, plaintext []byte) ([]byte, error) {
	key, err := e2ee.DecodeKey(sharedKeyBase64)
	if err != nil {
		return nil, fmt.Errorf("invalid shared key: %w", err)
	}
	return e2ee.Seal(key, plaintext)
}

// OpenPayload decrypts and authenticates ciphertext produced by SealPayload.
func OpenPayload(sharedKeyBase64 string, ciphertext []byte) ([]byte, error) {
	key, err := e2ee.DecodeKey(sharedKeyBase64)
	if err != nil {
		return nil, fmt.Errorf("invalid shared key: %w", err)
	}
	return e2ee.Open(key, ciphertext)
}

// BuildDirectoryManifestJSON hashes every file under dirPath into SaveVault's
// block-level delta manifest JSON.
func BuildDirectoryManifestJSON(dirPath string) (string, error) {
	if strings.TrimSpace(dirPath) == "" {
		return "", errors.New("dirPath is required")
	}
	m, err := delta.BuildManifest(dirPath)
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(m)
	return string(raw), err
}

// DiffManifestsJSON computes the block-level diff between two JSON manifests
// produced by BuildDirectoryManifestJSON or received from a SaveVault desktop peer.
func DiffManifestsJSON(localManifestJSON, remoteManifestJSON string) (string, error) {
	var local, remote delta.Manifest
	if err := json.Unmarshal([]byte(localManifestJSON), &local); err != nil {
		return "", fmt.Errorf("parse local manifest: %w", err)
	}
	if err := json.Unmarshal([]byte(remoteManifestJSON), &remote); err != nil {
		return "", fmt.Errorf("parse remote manifest: %w", err)
	}
	raw, err := json.Marshal(delta.DiffManifests(local, remote))
	return string(raw), err
}

// EncodeLanPing creates a wire-compatible UDP discovery datagram matching
// internal/p2p/discovery.
func EncodeLanPing(nodeID, deviceName, deviceType string, port int) (string, error) {
	nodeID = strings.TrimSpace(nodeID)
	if nodeID == "" {
		return "", errors.New("nodeID is required")
	}
	if port <= 0 || port > 65535 {
		return "", fmt.Errorf("invalid port %d", port)
	}
	deviceType = strings.TrimSpace(deviceType)
	if deviceType == "" {
		deviceType = "android"
	}
	raw, err := json.Marshal(discovery.Ping{
		Type:       "savevault-ping",
		NodeID:     nodeID,
		DeviceName: strings.TrimSpace(deviceName),
		DeviceType: deviceType,
		Port:       port,
	})
	return string(raw), err
}

// DecodeLanPing validates a UDP discovery datagram (supporting both
// "savevault-ping" and legacy "opensave-ping") and returns normalized JSON.
func DecodeLanPing(raw string) (string, error) {
	var p discovery.Ping
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return "", fmt.Errorf("invalid discovery ping: %w", err)
	}
	if p.Type != "savevault-ping" && p.Type != "opensave-ping" {
		return "", fmt.Errorf("unsupported ping type %q", p.Type)
	}
	p.NodeID = strings.TrimSpace(p.NodeID)
	p.DeviceName = strings.TrimSpace(p.DeviceName)
	p.DeviceType = strings.TrimSpace(p.DeviceType)
	if p.NodeID == "" || p.Port <= 0 || p.Port > 65535 {
		return "", errors.New("discovery ping missing nodeId or valid port")
	}
	out, err := json.Marshal(p)
	return string(out), err
}
