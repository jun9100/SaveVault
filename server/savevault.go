// SPDX-FileCopyrightText: Copyright 2026 Eden Emulator Project
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// SaveVault archives are ordinary ZIP files, unlike Eden's encrypted blobs.
// Keep native archives out of both objects/ and the legacy WebDAV collection so
// retention, garbage collection, and older clients cannot mistake one for another.
type saveVaultFile struct {
	Type        string    `json:"type"`
	Name        string    `json:"name"`
	SizeBytes   int64     `json:"sizeBytes"`
	CreatedTime time.Time `json:"createdTime"`
	SHA256      string    `json:"sha256,omitempty"`
	Device      string    `json:"device,omitempty"`
	DeviceName  string    `json:"deviceName,omitempty"`
	UploadedAt  time.Time `json:"uploadedAt,omitzero"`
}

// saveVaultCatalog records only transport-level metadata for native SaveVault
// archives. Save contents remain opaque to game-save; the checksum lets an
// administrator audit which exact archive a client uploaded without opening it.
type saveVaultCatalog struct {
	Files map[string]saveVaultFileMetadata `json:"files"`
}

type saveVaultFileMetadata struct {
	SHA256     string    `json:"sha256,omitempty"`
	Device     string    `json:"device,omitempty"`
	DeviceName string    `json:"device_name,omitempty"`
	UploadedAt time.Time `json:"uploaded_at,omitzero"`
}

func (c saveVaultCatalog) clone() saveVaultCatalog {
	next := saveVaultCatalog{Files: make(map[string]saveVaultFileMetadata, len(c.Files)+1)}
	for key, value := range c.Files {
		next.Files[key] = value
	}
	return next
}

func validSaveVaultType(value string) bool {
	if value == "pc" || value == "switch" {
		return true
	}
	return strings.HasPrefix(value, "emulator-") && validIdentifier(value, 64)
}

func validSaveVaultName(value string) bool {
	return validIdentifier(value, 255) && strings.HasSuffix(strings.ToLower(value), ".zip")
}

func (s *server) saveVaultNativeDirectory() string {
	return filepath.Join(s.dataDir, "savevault-native")
}

func (s *server) saveVaultCatalogPath() string {
	return filepath.Join(s.dataDir, "savevault-native.json")
}

func saveVaultCatalogKey(kind, name string) string {
	return kind + "\x00" + name
}

func (s *server) loadSaveVaultCatalog() error {
	if err := readJSONFile(s.saveVaultCatalogPath(), &s.saveVaultCatalog); err != nil {
		return err
	}
	if s.saveVaultCatalog.Files == nil {
		s.saveVaultCatalog.Files = make(map[string]saveVaultFileMetadata)
	}
	return nil
}

func (s *server) persistSaveVaultCatalog(catalog saveVaultCatalog) error {
	return writeJSONFile(s.saveVaultCatalogPath(), catalog)
}

func (s *server) saveVaultPath(kind, name string) (string, bool) {
	if !validSaveVaultType(kind) || !validSaveVaultName(name) {
		return "", false
	}
	return filepath.Join(s.saveVaultNativeDirectory(), kind, name), true
}

// saveVaultArchiveDigestLocked returns the digest of the bytes currently stored
// on disk. The caller holds saveVaultMu, so a successful comparison remains
// true until it has finished the associated state transition.
func (s *server) saveVaultArchiveDigestLocked(kind, name string) (string, error) {
	digest, _, err := s.saveVaultArchiveDigestWithInfo(kind, name)
	return digest, err
}

// saveVaultArchiveDigestWithInfo hashes an immutable archive and returns its
// stat info so callers can compute SHA-256 outside saveVaultMu and verify under
// the lock that the same file is still in place.
func (s *server) saveVaultArchiveDigestWithInfo(kind, name string) (string, os.FileInfo, error) {
	path, ok := s.saveVaultPath(kind, name)
	if !ok {
		return "", nil, errors.New("invalid SaveVault archive path")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", nil, err
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", nil, err
	}
	return hex.EncodeToString(hash.Sum(nil)), info, nil
}

func (s *server) handleSaveVaultList(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"files": s.listSaveVaultFiles()})
}

// listSaveVaultFiles returns only files currently present on disk. A manually
// removed archive therefore never remains visible because of stale catalog
// metadata, while older archives pre-dating the catalog remain listable.
func (s *server) listSaveVaultFiles() []saveVaultFile {
	s.saveVaultMu.Lock()
	defer s.saveVaultMu.Unlock()
	return s.listSaveVaultFilesLocked()
}

func (s *server) listSaveVaultFilesLocked() []saveVaultFile {
	root := s.saveVaultNativeDirectory()
	entries := []saveVaultFile{}
	for _, kind := range []string{"pc", "switch"} {
		entries = append(entries, s.saveVaultFilesForType(root, kind)...)
	}
	// Emulator types are extensible. Only immediate directories are accepted.
	if dirs, err := os.ReadDir(root); err == nil {
		for _, dir := range dirs {
			if dir.IsDir() && strings.HasPrefix(dir.Name(), "emulator-") && validSaveVaultType(dir.Name()) {
				entries = append(entries, s.saveVaultFilesForType(root, dir.Name())...)
			}
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Type != entries[j].Type {
			return entries[i].Type < entries[j].Type
		}
		return entries[i].Name < entries[j].Name
	})
	return entries
}

func (s *server) saveVaultFilesForType(root, kind string) []saveVaultFile {
	dir := filepath.Join(root, kind)
	items, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	files := make([]saveVaultFile, 0, len(items))
	for _, item := range items {
		if item.IsDir() || !validSaveVaultName(item.Name()) {
			continue
		}
		if info, err := item.Info(); err == nil {
			file := saveVaultFile{Type: kind, Name: item.Name(), SizeBytes: info.Size(), CreatedTime: info.ModTime().UTC()}
			if metadata, ok := s.saveVaultCatalog.Files[saveVaultCatalogKey(kind, item.Name())]; ok {
				file.SHA256 = metadata.SHA256
				file.Device = metadata.Device
				file.DeviceName = metadata.DeviceName
				file.UploadedAt = metadata.UploadedAt
			}
			files = append(files, file)
		}
	}
	return files
}

func (s *server) handleSaveVaultNativePut(w http.ResponseWriter, r *http.Request) {
	kind, name := r.PathValue("type"), r.PathValue("name")
	destination, ok := s.saveVaultPath(kind, name)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid SaveVault type or file name")
		return
	}
	if r.ContentLength > s.maxObjectBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "SaveVault archive exceeds configured size limit")
		return
	}
	// Reject a deleted or merged scope before reading the body or creating any
	// temporary file. The archive name embeds the scope game ID before "__".
	if gameID, found := saveVaultArchiveGameID(name); found {
		if tombstone, blocked := s.saveVaultScopeBlocked(kind, gameID); blocked {
			writeSaveVaultTombstone(w, tombstone)
			return
		}
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		writeError(w, http.StatusInternalServerError, "cannot create SaveVault storage")
		return
	}
	temporary, err := os.CreateTemp(filepath.Dir(destination), ".upload-*")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot create SaveVault upload")
		return
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		writeError(w, http.StatusInternalServerError, "cannot secure SaveVault upload")
		return
	}
	readLimit := s.maxObjectBytes
	quotaCapped := false
	if quota := s.getQuotaBytes(); quota > 0 && quota < readLimit {
		readLimit = quota
		quotaCapped = true
	}
	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(temporary, hash), http.MaxBytesReader(w, r.Body, readLimit+1))
	var syncErr error
	if copyErr == nil && written <= readLimit {
		syncErr = temporary.Sync()
	}
	if closeErr := temporary.Close(); closeErr != nil && copyErr == nil && syncErr == nil {
		syncErr = closeErr
	}
	if written > readLimit {
		if quotaCapped && written <= s.maxObjectBytes {
			writeError(w, http.StatusInsufficientStorage, "storage quota exceeded")
		} else {
			writeError(w, http.StatusRequestEntityTooLarge, "SaveVault archive exceeds configured size limit")
		}
		return
	}
	if copyErr != nil {
		var maxErr *http.MaxBytesError
		if errors.As(copyErr, &maxErr) {
			if quotaCapped && written <= s.maxObjectBytes {
				writeError(w, http.StatusInsufficientStorage, "storage quota exceeded")
			} else {
				writeError(w, http.StatusRequestEntityTooLarge, "SaveVault archive exceeds configured size limit")
			}
		} else {
			writeError(w, http.StatusBadRequest, "cannot read SaveVault upload")
		}
		return
	}
	if syncErr != nil {
		writeError(w, http.StatusInternalServerError, "cannot persist SaveVault upload")
		return
	}
	s.saveVaultMu.Lock()
	defer s.saveVaultMu.Unlock()
	// A delete may have started while the body was being received. Recheck under
	// saveVaultMu before publishing so the archive can never land after deletion.
	if gameID, found := saveVaultArchiveGameID(name); found {
		if tombstone, blocked := s.saveVaultScopeBlockedLocked(kind, gameID); blocked {
			writeSaveVaultTombstone(w, tombstone)
			return
		}
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	s.quotaMu.Lock()
	if _, statErr := os.Stat(destination); errors.Is(statErr, os.ErrNotExist) && !s.checkQuota(written) {
		s.quotaMu.Unlock()
		writeError(w, http.StatusInsufficientStorage, "storage quota exceeded")
		return
	}
	// Link publication is create-only: a second uploader can never replace an
	// archive that another request has already finished. The temporary file and
	// destination live in the same directory, so a successful link is atomic.
	// We deliberately fail closed on filesystems that cannot provide this
	// guarantee instead of falling back to a truncating write.
	linkErr := os.Link(temporaryPath, destination)
	if linkErr == nil {
		s.adjustUsedBytes(written)
	}
	s.quotaMu.Unlock()
	if linkErr != nil {
		err := linkErr
		if !errors.Is(err, os.ErrExist) {
			writeError(w, http.StatusInternalServerError, "cannot publish SaveVault archive")
			return
		}
		existingDigest, digestErr := s.saveVaultArchiveDigestLocked(kind, name)
		if digestErr != nil {
			writeError(w, http.StatusInternalServerError, "cannot verify existing SaveVault archive")
			return
		}
		if existingDigest == digest {
			// A lost response may make a client retry the same immutable archive.
			// Treat that as success without changing its original audit metadata.
			w.WriteHeader(http.StatusOK)
			return
		}
		writeError(w, http.StatusConflict, "SaveVault archive name already has different content")
		return
	}
	// The destination now has its own hard link. The deferred removal is a
	// second safeguard if this unlink fails during a transient filesystem error.
	_ = os.Remove(temporaryPath)

	metadata := saveVaultFileMetadata{
		SHA256:     digest,
		Device:     requestDeviceID(r),
		DeviceName: trimDeviceField(requestClientHeader(r, "X-Eden-Device-Name", "X-SaveVault-Device-Name"), 80),
		UploadedAt: time.Now().UTC(),
	}
	next := s.saveVaultCatalog.clone()
	next.Files[saveVaultCatalogKey(kind, name)] = metadata
	if err := s.persistSaveVaultCatalog(next); err != nil {
		// The archive itself is safely published. Do not turn a completed backup
		// into a client failure just because its advisory audit record is delayed.
		slog.Warn("persist SaveVault archive metadata", "type", kind, "name", name, "error", err)
	} else {
		s.saveVaultCatalog = next
	}
	w.WriteHeader(http.StatusCreated)
}

func (s *server) handleSaveVaultNativeGet(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	path, ok := s.saveVaultPath(r.PathValue("type"), name)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid SaveVault type or file name")
		return
	}
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, "SaveVault archive not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot open SaveVault archive")
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot inspect SaveVault archive")
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	http.ServeContent(w, r, name, info.ModTime(), file)
}

func (s *server) handleSaveVaultNativeDelete(w http.ResponseWriter, r *http.Request) {
	kind, name := r.PathValue("type"), r.PathValue("name")
	path, ok := s.saveVaultPath(kind, name)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid SaveVault type or file name")
		return
	}
	s.saveVaultMu.Lock()
	defer s.saveVaultMu.Unlock()
	if s.saveVaultArchiveReferencedLocked(kind, name) {
		writeError(w, http.StatusConflict, "SaveVault archive is referenced by a recoverable snapshot")
		return
	}
	var removedSize int64
	if info, statErr := os.Stat(path); statErr == nil && info.Mode().IsRegular() {
		removedSize = info.Size()
	}
	if err := os.Remove(path); errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, "SaveVault archive not found")
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot delete SaveVault archive")
	} else {
		s.adjustUsedBytes(-removedSize)
		next := s.saveVaultCatalog.clone()
		delete(next.Files, saveVaultCatalogKey(kind, name))
		if err := s.persistSaveVaultCatalog(next); err != nil {
			slog.Warn("persist SaveVault archive deletion", "type", kind, "name", name, "error", err)
		} else {
			s.saveVaultCatalog = next
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
