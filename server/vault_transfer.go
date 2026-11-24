// SPDX-FileCopyrightText: Copyright 2026 Eden Emulator Project
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	vaultExportManifestName = "manifest.json"
	maxVaultImportFiles     = 50000
	maxVaultImportBytes     = 2 << 30 // 2 GiB unpacked safety cap
)

var allowedVaultRootFiles = map[string]bool{
	"state.json":                  true,
	"dashboard.json":              true,
	"settings.json":               true,
	"events.json":                 true,
	"device-tokens.json":          true,
	"savevault-native.json":       true,
	"savevault-native-state.json": true,
	"savevault-manage.json":       true,
	"savevault-meta.json":         true,
	"profile-metadata.json":       true,
	"webhooks.json":               true,
}

var allowedVaultSubdirs = []string{
	"objects",
	"savevault-native",
	"savevault",
	"savevault-meta",
	"chunks",
}

type vaultExportFileEntry struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type vaultExportManifest struct {
	Version       int                    `json:"version"`
	VaultID       string                 `json:"vault_id"`
	ExportedAt    time.Time              `json:"exported_at"`
	ServerVersion string                 `json:"server_version"`
	Files         []vaultExportFileEntry `json:"files"`
}

func isAllowedVaultRelPath(rel string) bool {
	if rel == "" || strings.HasPrefix(rel, "/") || strings.Contains(rel, "\\") {
		return false
	}
	cleaned := path.Clean(rel)
	if cleaned != rel || cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return false
	}
	if allowedVaultRootFiles[cleaned] {
		return true
	}
	parts := strings.Split(cleaned, "/")
	if len(parts) < 2 {
		return false
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || strings.HasPrefix(part, ".") {
			return false
		}
	}
	for _, sub := range allowedVaultSubdirs {
		if parts[0] == sub {
			return true
		}
	}
	return false
}

func (s *server) collectVaultExportFiles() ([]string, error) {
	var rels []string
	for name := range allowedVaultRootFiles {
		full := filepath.Join(s.dataDir, name)
		info, err := os.Stat(full)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if info.Mode().IsRegular() {
			rels = append(rels, name)
		}
	}
	for _, sub := range allowedVaultSubdirs {
		subRoot := filepath.Join(s.dataDir, sub)
		if _, err := os.Stat(subRoot); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return nil, err
		}
		err := filepath.Walk(subRoot, func(full string, info os.FileInfo, walkErr error) error {
			if walkErr != nil {
				if errors.Is(walkErr, os.ErrNotExist) {
					return nil
				}
				return walkErr
			}
			if !info.Mode().IsRegular() || strings.HasPrefix(info.Name(), ".") {
				return nil
			}
			rel, relErr := filepath.Rel(s.dataDir, full)
			if relErr != nil {
				return relErr
			}
			slashRel := filepath.ToSlash(rel)
			if isAllowedVaultRelPath(slashRel) {
				rels = append(rels, slashRel)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Strings(rels)
	return rels, nil
}

func (s *server) handleExportVault(w http.ResponseWriter, _ *http.Request) {
	rels, err := s.collectVaultExportFiles()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot enumerate vault files")
		return
	}
	stamp := s.now().UTC().Format("20060102-150405")
	safeVault := s.vaultID
	if !validIdentifier(safeVault, 48) {
		safeVault = "vault"
	}
	filename := fmt.Sprintf("savevault-%s-%s.tar.gz", safeVault, stamp)
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	w.Header().Set("Cache-Control", "no-store")

	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)

	manifest := vaultExportManifest{
		Version:       1,
		VaultID:       s.vaultID,
		ExportedAt:    s.now().UTC(),
		ServerVersion: version,
		Files:         make([]vaultExportFileEntry, 0, len(rels)),
	}

	for _, rel := range rels {
		full := filepath.Join(s.dataDir, filepath.FromSlash(rel))
		f, openErr := os.Open(full)
		if openErr != nil {
			if errors.Is(openErr, os.ErrNotExist) {
				continue
			}
			slog.Warn("vault export open failed", "path", rel, "error", openErr)
			return
		}
		info, statErr := f.Stat()
		if statErr != nil || !info.Mode().IsRegular() {
			_ = f.Close()
			if statErr != nil {
				slog.Warn("vault export stat failed", "path", rel, "error", statErr)
				return
			}
			continue
		}
		hdr := &tar.Header{
			Name:    rel,
			Mode:    0o600,
			Size:    info.Size(),
			ModTime: info.ModTime().UTC(),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			_ = f.Close()
			slog.Warn("vault export write header", "path", rel, "error", err)
			return
		}
		hasher := sha256.New()
		written, copyErr := io.Copy(io.MultiWriter(tw, hasher), io.LimitReader(f, info.Size()))
		_ = f.Close()
		if copyErr != nil || written != info.Size() {
			slog.Warn("vault export stream file failed", "path", rel, "written", written, "expected", info.Size(), "error", copyErr)
			return
		}
		manifest.Files = append(manifest.Files, vaultExportFileEntry{
			Path:   rel,
			Size:   written,
			SHA256: hex.EncodeToString(hasher.Sum(nil)),
		})
	}

	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		slog.Warn("vault export marshal manifest", "error", err)
		return
	}
	if err := tw.WriteHeader(&tar.Header{
		Name:    vaultExportManifestName,
		Mode:    0o600,
		Size:    int64(len(manifestBytes)),
		ModTime: manifest.ExportedAt,
	}); err != nil {
		slog.Warn("vault export write manifest header", "error", err)
		return
	}
	if _, err := tw.Write(manifestBytes); err != nil {
		slog.Warn("vault export write manifest body", "error", err)
		return
	}
	if err := tw.Close(); err != nil {
		slog.Warn("vault export close tar", "error", err)
		return
	}
	if err := gz.Close(); err != nil {
		slog.Warn("vault export close gzip", "error", err)
	}
}

func (s *server) handleImportVault(w http.ResponseWriter, r *http.Request) {
	dryRun := strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("dryRun")), "true") ||
		strings.TrimSpace(r.URL.Query().Get("dryRun")) == "1"

	stageDir, err := os.MkdirTemp(s.dataDir, ".vault-import-*")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot allocate import staging directory")
		return
	}
	defer func() { _ = os.RemoveAll(stageDir) }()

	gz, err := gzip.NewReader(http.MaxBytesReader(w, r.Body, maxVaultImportBytes))
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "vault archive exceeds maximum import size")
		} else {
			writeError(w, http.StatusBadRequest, "invalid gzip archive")
		}
		return
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	extracted := make(map[string]vaultExportFileEntry)
	var manifest *vaultExportManifest
	var totalBytes int64
	var fileCount int

	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				writeError(w, http.StatusRequestEntityTooLarge, "vault archive exceeds maximum import size")
			} else {
				writeError(w, http.StatusBadRequest, "corrupted tar archive: "+err.Error())
			}
			return
		}
		if hdr.Typeflag == tar.TypeDir {
			continue
		}
		if hdr.Typeflag != tar.TypeReg {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("unsupported tar entry type for %q", hdr.Name))
			return
		}
		cleanName := path.Clean(strings.TrimPrefix(hdr.Name, "./"))
		if cleanName == vaultExportManifestName {
			if manifest != nil {
				writeError(w, http.StatusBadRequest, "duplicate manifest.json in vault archive")
				return
			}
			raw, readErr := io.ReadAll(io.LimitReader(tr, 16<<20))
			if readErr != nil {
				writeError(w, http.StatusBadRequest, "cannot read manifest.json")
				return
			}
			var parsed vaultExportManifest
			if err := json.Unmarshal(raw, &parsed); err != nil {
				writeError(w, http.StatusBadRequest, "invalid manifest.json")
				return
			}
			manifest = &parsed
			continue
		}
		if !isAllowedVaultRelPath(cleanName) {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("disallowed path in vault archive: %q", hdr.Name))
			return
		}
		if _, dup := extracted[cleanName]; dup {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("duplicate path in vault archive: %q", cleanName))
			return
		}
		fileCount++
		if fileCount > maxVaultImportFiles {
			writeError(w, http.StatusRequestEntityTooLarge, "vault archive contains too many files")
			return
		}
		if hdr.Size < 0 || totalBytes+hdr.Size > maxVaultImportBytes {
			writeError(w, http.StatusRequestEntityTooLarge, "vault archive exceeds maximum import size")
			return
		}
		target := filepath.Join(stageDir, filepath.FromSlash(cleanName))
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			writeError(w, http.StatusInternalServerError, "cannot prepare import subdirectory")
			return
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "cannot stage imported file")
			return
		}
		hasher := sha256.New()
		written, copyErr := io.Copy(io.MultiWriter(out, hasher), io.LimitReader(tr, maxVaultImportBytes-totalBytes+1))
		closeErr := out.Close()
		if copyErr != nil {
			var maxErr *http.MaxBytesError
			if errors.As(copyErr, &maxErr) {
				writeError(w, http.StatusRequestEntityTooLarge, "vault archive exceeds maximum import size")
			} else {
				writeError(w, http.StatusBadRequest, "failed to extract "+cleanName)
			}
			return
		}
		if closeErr != nil {
			writeError(w, http.StatusInternalServerError, "cannot flush staged file "+cleanName)
			return
		}
		totalBytes += written
		if totalBytes > maxVaultImportBytes {
			writeError(w, http.StatusRequestEntityTooLarge, "vault archive exceeds maximum import size")
			return
		}
		extracted[cleanName] = vaultExportFileEntry{
			Path:   cleanName,
			Size:   written,
			SHA256: hex.EncodeToString(hasher.Sum(nil)),
		}
	}

	if manifest == nil {
		writeError(w, http.StatusBadRequest, "vault archive is missing manifest.json")
		return
	}
	if manifest.Version != 1 {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("unsupported vault archive manifest version %d", manifest.Version))
		return
	}
	seenManifest := make(map[string]struct{}, len(manifest.Files))
	for _, expected := range manifest.Files {
		if _, dup := seenManifest[expected.Path]; dup {
			writeError(w, http.StatusUnprocessableEntity, fmt.Sprintf("duplicate manifest entry: %s", expected.Path))
			return
		}
		seenManifest[expected.Path] = struct{}{}
		actual, ok := extracted[expected.Path]
		if !ok {
			writeError(w, http.StatusUnprocessableEntity, fmt.Sprintf("missing file listed in manifest: %s", expected.Path))
			return
		}
		if actual.Size != expected.Size || !strings.EqualFold(actual.SHA256, expected.SHA256) {
			writeError(w, http.StatusUnprocessableEntity, fmt.Sprintf("checksum mismatch for %s", expected.Path))
			return
		}
	}
	if len(seenManifest) != len(extracted) {
		writeError(w, http.StatusUnprocessableEntity, fmt.Sprintf("manifest lists %d files but archive contained %d", len(seenManifest), len(extracted)))
		return
	}

	if dryRun {
		writeJSON(w, http.StatusOK, map[string]any{
			"dry_run":         true,
			"valid":           true,
			"source_vault_id": manifest.VaultID,
			"exported_at":     manifest.ExportedAt,
			"files":           len(extracted),
			"total_bytes":     totalBytes,
		})
		return
	}

	if !s.maintaining.TryLock() {
		writeError(w, http.StatusConflict, "another maintenance or import operation is already running")
		return
	}
	defer s.maintaining.Unlock()

	// Remove root state JSON files absent from the backup so stale state does
	// not survive import, then install staged files in deterministic order.
	for name := range allowedVaultRootFiles {
		if _, present := extracted[name]; !present {
			_ = os.Remove(filepath.Join(s.dataDir, name))
		}
	}
	ordered := make([]string, 0, len(extracted))
	for rel := range extracted {
		ordered = append(ordered, rel)
	}
	sort.Strings(ordered)
	appliedFiles := 0
	for _, rel := range ordered {
		src := filepath.Join(stageDir, filepath.FromSlash(rel))
		dst := filepath.Join(s.dataDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			s.reconcileUsedBytes()
			writeError(w, http.StatusInternalServerError, "cannot create destination directory during import")
			return
		}
		if err := os.Rename(src, dst); err != nil {
			s.reconcileUsedBytes()
			writeError(w, http.StatusInternalServerError, "cannot install imported file "+rel)
			return
		}
		appliedFiles++
	}
	if err := s.reloadVaultStateAfterImport(); err != nil {
		s.reconcileUsedBytes()
		slog.Error("reload vault state after import failed", "vault", s.vaultID, "error", err)
		writeError(w, http.StatusInternalServerError, "imported files but failed to reload vault state: "+err.Error())
		return
	}
	used := s.reconcileUsedBytes()
	writeJSON(w, http.StatusOK, map[string]any{
		"dry_run":         false,
		"applied":         true,
		"source_vault_id": manifest.VaultID,
		"exported_at":     manifest.ExportedAt,
		"files":           appliedFiles,
		"total_bytes":     totalBytes,
		"used_bytes":      used,
	})
}

func (s *server) reloadVaultStateAfterImport() error {
	s.mu.Lock()
	if err := s.loadState(); err != nil {
		s.mu.Unlock()
		return err
	}
	if err := s.loadDashboard(); err != nil {
		s.mu.Unlock()
		return err
	}
	if err := s.loadSettings(serverSettings{}); err != nil {
		s.mu.Unlock()
		return err
	}
	if err := s.loadEvents(); err != nil {
		s.mu.Unlock()
		return err
	}
	s.mu.Unlock()

	s.tokensMu.Lock()
	if err := s.loadDeviceTokens(); err != nil {
		s.tokensMu.Unlock()
		return err
	}
	s.tokensMu.Unlock()

	s.saveVaultMu.Lock()
	if err := s.loadSaveVaultCatalog(); err != nil {
		s.saveVaultMu.Unlock()
		return err
	}
	if err := s.loadSaveVaultState(); err != nil {
		s.saveVaultMu.Unlock()
		return err
	}
	s.saveVaultMu.Unlock()

	s.manageMu.Lock()
	if err := s.loadSaveVaultManage(); err != nil {
		s.manageMu.Unlock()
		return err
	}
	s.manageMu.Unlock()

	s.metaMu.Lock()
	if err := s.loadSaveVaultMeta(); err != nil {
		s.metaMu.Unlock()
		return err
	}
	s.metaMu.Unlock()

	if err := s.loadProfileMetadata(); err != nil {
		return err
	}
	if err := s.loadWebhooks(); err != nil {
		return err
	}
	return nil
}
