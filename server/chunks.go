// SPDX-FileCopyrightText: Copyright 2026 Eden Emulator Project
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	chunksDirName        = "chunks"
	maxChunkSizeBytes    = 4 * 1024 * 1024 // 4 MiB per individual content-addressed chunk
	maxMissingCheckBatch = 4096
	maxAssembleChunks    = 8192

	// FastCDC parameters (min 16 KiB, avg 64 KiB, max 256 KiB).
	fastCDCMinSize = 16 * 1024
	fastCDCAvgSize = 64 * 1024
	fastCDCMaxSize = 256 * 1024

	// Normalized chunking masks: tighter (more 1-bits) before avg size to curb
	// tiny chunks, looser after avg size to stabilize upper tail.
	fastCDCMaskS uint64 = 0x0000d90303530000 // ~18 bits
	fastCDCMaskL uint64 = 0x0000d90003530000 // ~14 bits
)

var fastCDCGearTable [256]uint64

func init() {
	// Deterministic SplitMix64 table for Gear rolling hash (zero external deps).
	state := uint64(0x536176655661756c) // "SaveVaul"
	for i := range fastCDCGearTable {
		state += 0x9e3779b97f4a7c15
		z := state
		z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
		z = (z ^ (z >> 27)) * 0x94d049bb133111eb
		fastCDCGearTable[i] = z ^ (z >> 31)
	}
}

type fastCDCChunk struct {
	Offset int
	Length int
	SHA256 string
	Data   []byte
}

// splitFastCDC splits data into content-defined chunks using normalized Gear
// hashing so insertions/deletions at arbitrary offsets preserve surrounding
// chunk boundaries for high deduplication ratios.
func splitFastCDC(data []byte) []fastCDCChunk {
	if len(data) == 0 {
		return nil
	}
	var chunks []fastCDCChunk
	offset := 0
	for offset < len(data) {
		remaining := len(data) - offset
		cut := nextFastCDCCut(data[offset:], remaining)
		slice := data[offset : offset+cut]
		sum := sha256.Sum256(slice)
		chunks = append(chunks, fastCDCChunk{
			Offset: offset,
			Length: cut,
			SHA256: hex.EncodeToString(sum[:]),
			Data:   slice,
		})
		offset += cut
	}
	return chunks
}

func nextFastCDCCut(buf []byte, remaining int) int {
	if remaining <= fastCDCMinSize {
		return remaining
	}
	limit := remaining
	if limit > fastCDCMaxSize {
		limit = fastCDCMaxSize
	}
	normal := fastCDCAvgSize
	if normal > limit {
		normal = limit
	}
	var fp uint64
	i := fastCDCMinSize
	for ; i < normal; i++ {
		fp = (fp << 1) + fastCDCGearTable[buf[i]]
		if (fp & fastCDCMaskS) == 0 {
			return i + 1
		}
	}
	for ; i < limit; i++ {
		fp = (fp << 1) + fastCDCGearTable[buf[i]]
		if (fp & fastCDCMaskL) == 0 {
			return i + 1
		}
	}
	return limit
}

func (s *server) chunksRoot() string {
	return filepath.Join(s.dataDir, chunksDirName)
}

func (s *server) chunkPath(hash string) (string, bool) {
	if !validObjectID(hash) {
		return "", false
	}
	return filepath.Join(s.chunksRoot(), hash[:2], hash), true
}

func (s *server) handleChunksMissing(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Hashes []string `json:"hashes"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 512*1024)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid chunk query body")
		return
	}
	if len(body.Hashes) > maxMissingCheckBatch {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("at most %d chunk hashes per request", maxMissingCheckBatch))
		return
	}
	missing := make([]string, 0, len(body.Hashes))
	present := make([]string, 0, len(body.Hashes))
	seen := make(map[string]struct{}, len(body.Hashes))
	now := s.now().UTC()
	for _, raw := range body.Hashes {
		h := strings.ToLower(strings.TrimSpace(raw))
		path, valid := s.chunkPath(h)
		if !valid {
			writeError(w, http.StatusBadRequest, "invalid chunk hash")
			return
		}
		if _, dup := seen[h]; dup {
			continue
		}
		seen[h] = struct{}{}
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
			_ = os.Chtimes(path, now, now)
			present = append(present, h)
		} else {
			missing = append(missing, h)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"missing": missing,
		"present": present,
	})
}

func (s *server) handlePutChunk(w http.ResponseWriter, r *http.Request) {
	hash := strings.ToLower(strings.TrimSpace(r.PathValue("hash")))
	dest, ok := s.chunkPath(hash)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid chunk hash")
		return
	}
	if info, err := os.Stat(dest); err == nil && info.Mode().IsRegular() {
		now := s.now().UTC()
		_ = os.Chtimes(dest, now, now)
		_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, maxChunkSizeBytes+1))
		writeJSON(w, http.StatusOK, map[string]any{
			"hash":   hash,
			"size":   info.Size(),
			"status": "exists",
		})
		return
	}
	dir := filepath.Dir(dest)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		writeError(w, http.StatusInternalServerError, "cannot prepare chunk directory")
		return
	}
	tmp, err := os.CreateTemp(dir, ".chunk-*")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot stage chunk upload")
		return
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()

	readLimit := int64(maxChunkSizeBytes)
	quotaCapped := false
	if quota := s.getQuotaBytes(); quota > 0 && quota < readLimit {
		readLimit = quota
		quotaCapped = true
	}
	h := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(tmp, h), http.MaxBytesReader(w, r.Body, readLimit))
	var syncErr error
	if copyErr == nil {
		syncErr = tmp.Sync()
	}
	if closeErr := tmp.Close(); closeErr != nil && copyErr == nil && syncErr == nil {
		syncErr = closeErr
	}
	if copyErr != nil {
		var maxErr *http.MaxBytesError
		if errors.As(copyErr, &maxErr) {
			if quotaCapped {
				writeError(w, http.StatusInsufficientStorage, "storage quota exceeded")
			} else {
				writeError(w, http.StatusRequestEntityTooLarge, "chunk exceeds configured size limit")
			}
		} else {
			writeError(w, http.StatusBadRequest, "cannot read chunk upload")
		}
		return
	}
	if syncErr != nil {
		writeError(w, http.StatusInternalServerError, "cannot persist chunk")
		return
	}
	if actual := hex.EncodeToString(h.Sum(nil)); actual != hash {
		writeError(w, http.StatusUnprocessableEntity, "chunk sha256 does not match hash")
		return
	}

	s.quotaMu.Lock()
	if _, statErr := os.Stat(dest); errors.Is(statErr, os.ErrNotExist) && !s.checkQuota(written) {
		s.quotaMu.Unlock()
		writeError(w, http.StatusInsufficientStorage, "storage quota exceeded")
		return
	}
	linkErr := os.Link(tmpPath, dest)
	if linkErr == nil {
		s.adjustUsedBytes(written)
	}
	s.quotaMu.Unlock()

	if linkErr != nil {
		if errors.Is(linkErr, os.ErrExist) {
			writeJSON(w, http.StatusOK, map[string]any{
				"hash":   hash,
				"size":   written,
				"status": "exists",
			})
			return
		}
		writeError(w, http.StatusInternalServerError, "cannot publish chunk")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"hash":   hash,
		"size":   written,
		"status": "stored",
	})
}

func (s *server) handleGetChunk(w http.ResponseWriter, r *http.Request) {
	hash := strings.ToLower(strings.TrimSpace(r.PathValue("hash")))
	path, ok := s.chunkPath(hash)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid chunk hash")
		return
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, "chunk not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot open chunk")
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot inspect chunk")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("ETag", `"`+hash+`"`)
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	http.ServeContent(w, r, hash, info.ModTime(), f)
}

// handleAssembleChunks assembles a standard immutable SaveVault archive from
// previously uploaded content-addressed chunks, verifying every chunk digest
// and the combined archive digest so WebDAV and legacy clients stay 100%
// compatible.
func (s *server) handleAssembleChunks(w http.ResponseWriter, r *http.Request) {
	kind, name := r.PathValue("type"), r.PathValue("name")
	dest, ok := s.saveVaultPath(kind, name)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid SaveVault type or file name")
		return
	}
	if gameID, found := saveVaultArchiveGameID(name); found {
		if tombstone, blocked := s.saveVaultScopeBlocked(kind, gameID); blocked {
			writeSaveVaultTombstone(w, tombstone)
			return
		}
	}
	var body struct {
		Chunks         []string `json:"chunks"`
		ExpectedSHA256 string   `json:"expected_sha256,omitempty"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024*1024)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid assemble request body")
		return
	}
	if len(body.Chunks) == 0 {
		writeError(w, http.StatusBadRequest, "at least one chunk hash is required")
		return
	}
	if len(body.Chunks) > maxAssembleChunks {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("at most %d chunks per archive", maxAssembleChunks))
		return
	}
	expected := strings.ToLower(strings.TrimSpace(body.ExpectedSHA256))
	if expected != "" && !validObjectID(expected) {
		writeError(w, http.StatusBadRequest, "invalid expected_sha256")
		return
	}

	chunkPaths := make([]string, 0, len(body.Chunks))
	chunkHashes := make([]string, 0, len(body.Chunks))
	var missing []string
	missingSeen := make(map[string]struct{})
	var totalEstimated int64
	quota := s.getQuotaBytes()
	for _, raw := range body.Chunks {
		h := strings.ToLower(strings.TrimSpace(raw))
		path, valid := s.chunkPath(h)
		if !valid {
			writeError(w, http.StatusBadRequest, "invalid chunk hash")
			return
		}
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			if _, dup := missingSeen[h]; !dup {
				missingSeen[h] = struct{}{}
				missing = append(missing, h)
			}
			continue
		}
		totalEstimated += info.Size()
		if totalEstimated > s.maxObjectBytes {
			writeError(w, http.StatusRequestEntityTooLarge, "assembled archive exceeds configured size limit")
			return
		}
		if quota > 0 && s.usedBytes()+totalEstimated > quota {
			writeError(w, http.StatusInsufficientStorage, "storage quota exceeded")
			return
		}
		chunkPaths = append(chunkPaths, path)
		chunkHashes = append(chunkHashes, h)
	}
	if len(missing) > 0 {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":   "missing required chunks",
			"missing": missing,
		})
		return
	}

	dir := filepath.Dir(dest)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		writeError(w, http.StatusInternalServerError, "cannot prepare archive directory")
		return
	}
	tmp, err := os.CreateTemp(dir, ".assemble-*")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot stage assembled archive")
		return
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()

	archiveHash := sha256.New()
	var totalWritten int64
	for idx, cPath := range chunkPaths {
		cf, openErr := os.Open(cPath)
		if openErr != nil {
			_ = tmp.Close()
			writeError(w, http.StatusConflict, "chunk disappeared during assembly")
			return
		}
		chunkHasher := sha256.New()
		n, copyErr := io.Copy(io.MultiWriter(tmp, archiveHash, chunkHasher), io.LimitReader(cf, maxChunkSizeBytes+1))
		_ = cf.Close()
		if copyErr != nil {
			_ = tmp.Close()
			writeError(w, http.StatusInternalServerError, "cannot read chunk during assembly")
			return
		}
		if hex.EncodeToString(chunkHasher.Sum(nil)) != chunkHashes[idx] {
			_ = tmp.Close()
			writeError(w, http.StatusInternalServerError, "corrupted chunk detected during assembly")
			return
		}
		totalWritten += n
		if totalWritten > s.maxObjectBytes {
			_ = tmp.Close()
			writeError(w, http.StatusRequestEntityTooLarge, "assembled archive exceeds configured size limit")
			return
		}
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		writeError(w, http.StatusInternalServerError, "cannot flush assembled archive")
		return
	}
	if err := tmp.Close(); err != nil {
		writeError(w, http.StatusInternalServerError, "cannot close assembled archive")
		return
	}

	digest := hex.EncodeToString(archiveHash.Sum(nil))
	if expected != "" && digest != expected {
		writeError(w, http.StatusUnprocessableEntity, "assembled archive sha256 does not match expected_sha256")
		return
	}

	s.saveVaultMu.Lock()
	defer s.saveVaultMu.Unlock()
	if gameID, found := saveVaultArchiveGameID(name); found {
		if tombstone, blocked := s.saveVaultScopeBlockedLocked(kind, gameID); blocked {
			writeSaveVaultTombstone(w, tombstone)
			return
		}
	}
	s.quotaMu.Lock()
	if _, statErr := os.Stat(dest); errors.Is(statErr, os.ErrNotExist) && !s.checkQuota(totalWritten) {
		s.quotaMu.Unlock()
		writeError(w, http.StatusInsufficientStorage, "storage quota exceeded")
		return
	}
	linkErr := os.Link(tmpPath, dest)
	if linkErr == nil {
		s.adjustUsedBytes(totalWritten)
	}
	s.quotaMu.Unlock()

	if linkErr != nil {
		if !errors.Is(linkErr, os.ErrExist) {
			writeError(w, http.StatusInternalServerError, "cannot publish assembled archive")
			return
		}
		existingDigest, digestErr := s.saveVaultArchiveDigestLocked(kind, name)
		if digestErr != nil {
			writeError(w, http.StatusInternalServerError, "cannot verify existing SaveVault archive")
			return
		}
		if existingDigest == digest {
			writeJSON(w, http.StatusOK, map[string]any{
				"archive": name,
				"size":    totalWritten,
				"sha256":  digest,
				"chunks":  len(chunkHashes),
				"status":  "exists",
			})
			return
		}
		writeError(w, http.StatusConflict, "SaveVault archive name already has different content")
		return
	}
	_ = os.Remove(tmpPath)

	metadata := saveVaultFileMetadata{
		SHA256:     digest,
		Device:     requestDeviceID(r),
		DeviceName: trimDeviceField(requestClientHeader(r, "X-Eden-Device-Name", "X-SaveVault-Device-Name"), 80),
		UploadedAt: s.now().UTC(),
	}
	next := s.saveVaultCatalog.clone()
	next.Files[saveVaultCatalogKey(kind, name)] = metadata
	if err := s.persistSaveVaultCatalog(next); err != nil {
		slog.Warn("persist assembled SaveVault archive metadata", "type", kind, "name", name, "error", err)
	} else {
		s.saveVaultCatalog = next
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"archive": name,
		"size":    totalWritten,
		"sha256":  digest,
		"chunks":  len(chunkHashes),
		"status":  "assembled",
	})
}

func (s *server) chunkStoreStats() (count int, bytes int64) {
	_ = filepath.Walk(s.chunksRoot(), func(_ string, info os.FileInfo, err error) error {
		if err != nil || info == nil || !info.Mode().IsRegular() {
			return nil
		}
		if strings.HasPrefix(info.Name(), ".") {
			return nil
		}
		count++
		bytes += info.Size()
		return nil
	})
	return count, bytes
}

func (s *server) handleGetChunkStats(w http.ResponseWriter, _ *http.Request) {
	count, bytes := s.chunkStoreStats()
	writeJSON(w, http.StatusOK, map[string]any{
		"chunks":      count,
		"bytes":       bytes,
		"used_bytes":  s.usedBytes(),
		"quota_bytes": s.getQuotaBytes(),
		"reconciles":  s.usedBytesReconciles.Load(),
	})
}

func (s *server) handleGCChunks(w http.ResponseWriter, r *http.Request) {
	minAgeSeconds := int64(3600)
	if r.Body != nil && r.ContentLength != 0 {
		var body struct {
			MinAgeSeconds *int64 `json:"min_age_seconds"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			writeError(w, http.StatusBadRequest, "invalid gc request body")
			return
		}
		if body.MinAgeSeconds != nil {
			if *body.MinAgeSeconds < 0 {
				writeError(w, http.StatusBadRequest, "min_age_seconds must be non-negative")
				return
			}
			minAgeSeconds = *body.MinAgeSeconds
		}
	}
	cutoff := s.now().UTC().Add(-time.Duration(minAgeSeconds) * time.Second)
	var removed int
	var freed int64
	_ = filepath.Walk(s.chunksRoot(), func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || !info.Mode().IsRegular() {
			return nil
		}
		if strings.HasPrefix(info.Name(), ".") {
			return nil
		}
		if minAgeSeconds > 0 && info.ModTime().After(cutoff) {
			return nil
		}
		size := info.Size()
		if removeErr := os.Remove(path); removeErr == nil {
			removed++
			freed += size
		}
		return nil
	})
	actual := s.reconcileUsedBytes()
	writeJSON(w, http.StatusOK, map[string]any{
		"removed_chunks": removed,
		"freed_bytes":    freed,
		"used_bytes":     actual,
	})
}
