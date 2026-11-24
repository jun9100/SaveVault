package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	secretPrefix   = "svsec1:"
	secretKeyBytes = 32
	secretKeyFile  = ".secret.key"
)

func (s *Store) loadOrCreateSecretKey() ([]byte, error) {
	s.keyMu.Lock()
	defer s.keyMu.Unlock()
	if len(s.secretKey) == secretKeyBytes {
		return s.secretKey, nil
	}
	// In-memory test databases keep an ephemeral process-local key.
	if s.dbPath == "" || s.dbPath == ":memory:" || strings.HasPrefix(s.dbPath, "file::memory:") {
		key := make([]byte, secretKeyBytes)
		if _, err := io.ReadFull(rand.Reader, key); err != nil {
			return nil, fmt.Errorf("generate secret key: %w", err)
		}
		s.secretKey = key
		return key, nil
	}
	keyPath := filepath.Join(filepath.Dir(s.dbPath), secretKeyFile)
	if raw, err := os.ReadFile(keyPath); err == nil {
		decoded, decErr := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
		if decErr == nil && len(decoded) == secretKeyBytes {
			restrictToOwner(keyPath)
			s.secretKey = decoded
			return decoded, nil
		}
	}
	key := make([]byte, secretKeyBytes)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, fmt.Errorf("generate secret key: %w", err)
	}
	encoded := []byte(base64.StdEncoding.EncodeToString(key))
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		return nil, fmt.Errorf("create secret key dir: %w", err)
	}
	f, err := os.OpenFile(keyPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			// Another process created the key concurrently; read it back.
			raw, readErr := os.ReadFile(keyPath)
			if readErr != nil {
				return nil, fmt.Errorf("read existing secret key: %w", readErr)
			}
			decoded, decErr := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
			if decErr != nil || len(decoded) != secretKeyBytes {
				return nil, fmt.Errorf("invalid secret key file")
			}
			s.secretKey = decoded
			return decoded, nil
		}
		return nil, fmt.Errorf("write secret key: %w", err)
	}
	_, writeErr := f.Write(encoded)
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil {
		_ = os.Remove(keyPath)
		if writeErr != nil {
			return nil, fmt.Errorf("write secret key: %w", writeErr)
		}
		return nil, fmt.Errorf("close secret key: %w", closeErr)
	}
	restrictToOwner(keyPath)
	s.secretKey = key
	return key, nil
}

func (s *Store) encryptSecret(plaintext string) (string, error) {
	if plaintext == "" || strings.HasPrefix(plaintext, secretPrefix) {
		return plaintext, nil
	}
	key, err := s.loadOrCreateSecretKey()
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return secretPrefix + base64.StdEncoding.EncodeToString(sealed), nil
}

func (s *Store) decryptSecret(stored string) (string, error) {
	if !strings.HasPrefix(stored, secretPrefix) {
		return stored, nil // legacy plaintext column — transparently readable
	}
	key, err := s.loadOrCreateSecretKey()
	if err != nil {
		return "", err
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(stored, secretPrefix))
	if err != nil {
		return "", fmt.Errorf("decode encrypted secret: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", fmt.Errorf("truncated encrypted secret")
	}
	plain, err := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], nil)
	if err != nil {
		return "", fmt.Errorf("decrypt secret: %w", err)
	}
	return string(plain), nil
}
