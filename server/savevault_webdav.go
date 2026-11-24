// SPDX-FileCopyrightText: Copyright 2026 Eden Emulator Project
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const saveVaultWebDAVAllow = "OPTIONS, PROPFIND, PUT, HEAD, GET, DELETE, MOVE"

// saveVaultEnabled is deliberately independent of the Eden sync token. SaveVault
// stores ordinary ZIP snapshots, while Eden's /v1 object store contains encrypted
// blobs with a different lifecycle and authorization model.
func (s *server) saveVaultEnabled() bool {
	return s.saveVaultUsername != "" && s.saveVaultPassword != ""
}

func (s *server) requireSaveVaultAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		now := s.now()
		address := s.clientIP(r)
		if s.webdavLogins.blocked(address, now) {
			writeError(w, http.StatusTooManyRequests, "too many failed attempts, try again later")
			return
		}
		username, password, ok := r.BasicAuth()
		// Compare fixed-size digests so neither credential's length changes the
		// comparison work. The credentials themselves are never logged.
		gotUser, wantUser := sha256.Sum256([]byte(username)), sha256.Sum256([]byte(s.saveVaultUsername))
		gotPassword, wantPassword := sha256.Sum256([]byte(password)), sha256.Sum256([]byte(s.saveVaultPassword))
		if !ok || subtle.ConstantTimeCompare(gotUser[:], wantUser[:]) != 1 ||
			subtle.ConstantTimeCompare(gotPassword[:], wantPassword[:]) != 1 {
			if ok {
				s.webdavLogins.fail(address, now)
			}
			w.Header().Set("WWW-Authenticate", `Basic realm="GAME_SAVE SaveVault", charset="UTF-8"`)
			writeError(w, http.StatusUnauthorized, "SaveVault WebDAV authentication required")
			return
		}
		s.webdavLogins.reset(address)
		next(w, r)
	}
}

func (s *server) handleSaveVaultWebDAV(w http.ResponseWriter, r *http.Request) {
	root, name, err := saveVaultResourceRootAndName(r.URL.Path, r.URL.EscapedPath())
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	switch r.Method {
	case http.MethodOptions:
		w.Header().Set("Allow", saveVaultWebDAVAllow)
		w.Header().Set("DAV", "1")
		w.Header().Set("MS-Author-Via", "DAV")
		w.WriteHeader(http.StatusNoContent)
	case "PROPFIND":
		s.handleSaveVaultPropfind(w, r, root, name)
	case http.MethodPut:
		if name == "" {
			s.saveVaultMethodNotAllowed(w)
			return
		}
		s.handleSaveVaultPut(w, r, name)
	case http.MethodHead, http.MethodGet:
		if name == "" {
			s.saveVaultMethodNotAllowed(w)
			return
		}
		s.handleSaveVaultGet(w, r, name)
	case http.MethodDelete:
		if name == "" {
			s.saveVaultMethodNotAllowed(w)
			return
		}
		s.handleSaveVaultDelete(w, name)
	case "MOVE":
		if name == "" {
			s.saveVaultMethodNotAllowed(w)
			return
		}
		s.handleSaveVaultMove(w, r, name)
	default:
		s.saveVaultMethodNotAllowed(w)
	}
}

func (s *server) saveVaultMethodNotAllowed(w http.ResponseWriter) {
	w.Header().Set("Allow", saveVaultWebDAVAllow)
	writeError(w, http.StatusMethodNotAllowed, "method is not supported by the SaveVault WebDAV endpoint")
}

// saveVaultResourceRootAndName accepts only one safe filename beneath
// /savevault (or its legacy /opensave alias). The protocol never exposes
// folders: snapshots and temporary .part files sit in a single private
// directory, which makes path traversal impossible by design.
func saveVaultResourceRootAndName(requestPath, escapedPath string) (string, string, error) {
	escapedPath = strings.ToLower(escapedPath)
	if strings.Contains(escapedPath, "%2f") || strings.Contains(escapedPath, "%5c") {
		return "", "", errors.New("encoded path separators are not allowed")
	}
	for _, root := range []string{"/savevault", "/opensave"} {
		if requestPath == root || requestPath == root+"/" {
			return root, "", nil
		}
		if strings.HasPrefix(requestPath, root+"/") {
			name := strings.TrimPrefix(requestPath, root+"/")
			if !validSaveVaultFileName(name) {
				return "", "", errors.New("invalid SaveVault snapshot name")
			}
			return root, name, nil
		}
	}
	return "", "", errors.New("invalid SaveVault WebDAV path")
}

func saveVaultResourceName(requestPath, escapedPath string) (string, error) {
	_, name, err := saveVaultResourceRootAndName(requestPath, escapedPath)
	return name, err
}

func validSaveVaultFileName(name string) bool {
	if name == "" || len(name) > 240 || name == "." || name == ".." || strings.HasPrefix(name, ".upload-") {
		return false
	}
	for _, character := range name {
		if (character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') ||
			character == '.' || character == '-' || character == '_' {
			continue
		}
		return false
	}
	return true
}

func (s *server) saveVaultDirectory() string {
	return filepath.Join(s.dataDir, "savevault")
}

func (s *server) saveVaultFilePath(name string) string {
	return filepath.Join(s.saveVaultDirectory(), name)
}

func (s *server) handleSaveVaultPropfind(w http.ResponseWriter, r *http.Request, root, name string) {
	depth := strings.TrimSpace(r.Header.Get("Depth"))
	if depth == "" {
		depth = "0"
	}
	if depth != "0" && depth != "1" {
		writeError(w, http.StatusForbidden, "only WebDAV Depth 0 and 1 are supported")
		return
	}

	entries := []saveVaultDAVEntry{}
	if name != "" {
		info, err := saveVaultRegularFileInfo(s.saveVaultFilePath(name))
		if errors.Is(err, fs.ErrNotExist) {
			writeError(w, http.StatusNotFound, "snapshot not found")
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "cannot inspect snapshot")
			return
		}
		entries = append(entries, saveVaultDAVEntry{
			href: root + "/" + name,
			info: info,
		})
	} else {
		entries = append(entries, saveVaultDAVEntry{href: root + "/", collection: true})
		if depth == "1" {
			listed, err := s.saveVaultFiles(root)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "cannot list SaveVault snapshots")
				return
			}
			entries = append(entries, listed...)
		}
	}

	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusMultiStatus)
	_, _ = io.WriteString(w, `<?xml version="1.0" encoding="utf-8"?><D:multistatus xmlns:D="DAV:">`)
	for _, entry := range entries {
		writeSaveVaultDAVEntry(w, entry)
	}
	_, _ = io.WriteString(w, `</D:multistatus>`)
}

type saveVaultDAVEntry struct {
	href       string
	info       os.FileInfo
	collection bool
}

func (s *server) saveVaultFiles(root string) ([]saveVaultDAVEntry, error) {
	directory, err := os.ReadDir(s.saveVaultDirectory())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	entries := make([]saveVaultDAVEntry, 0, len(directory))
	for _, entry := range directory {
		name := entry.Name()
		if !validSaveVaultFileName(name) || strings.HasSuffix(name, ".part") {
			continue
		}
		info, err := saveVaultRegularFileInfo(filepath.Join(s.saveVaultDirectory(), name))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		entries = append(entries, saveVaultDAVEntry{href: root + "/" + name, info: info})
	}
	sort.Slice(entries, func(left, right int) bool { return entries[left].href < entries[right].href })
	return entries, nil
}

func writeSaveVaultDAVEntry(w io.Writer, entry saveVaultDAVEntry) {
	_, _ = io.WriteString(w, `<D:response><D:href>`)
	_ = xml.EscapeText(w, []byte(entry.href))
	_, _ = io.WriteString(w, `</D:href><D:propstat><D:prop>`)
	if entry.collection {
		_, _ = io.WriteString(w, `<D:resourcetype><D:collection/></D:resourcetype>`)
	} else {
		_, _ = fmt.Fprintf(w, `<D:getcontentlength>%d</D:getcontentlength><D:getlastmodified>%s</D:getlastmodified><D:resourcetype/>`,
			entry.info.Size(), entry.info.ModTime().UTC().Format(http.TimeFormat))
	}
	_, _ = io.WriteString(w, `</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`)
}

func (s *server) handleSaveVaultPut(w http.ResponseWriter, r *http.Request, name string) {
	if r.ContentLength > s.maxObjectBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "snapshot exceeds configured size limit")
		return
	}
	destination := s.saveVaultFilePath(name)
	_, statErr := os.Stat(destination)
	if statErr != nil && !errors.Is(statErr, fs.ErrNotExist) {
		writeError(w, http.StatusInternalServerError, "cannot inspect destination snapshot")
		return
	}

	temporary, err := os.CreateTemp(s.saveVaultDirectory(), ".upload-*.part")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot create temporary snapshot")
		return
	}
	temporaryPath := temporary.Name()
	published := false
	defer func() {
		if !published {
			_ = os.Remove(temporaryPath)
		}
	}()

	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		writeError(w, http.StatusInternalServerError, "cannot secure temporary snapshot")
		return
	}
	written, copyErr := io.Copy(temporary, http.MaxBytesReader(w, r.Body, s.maxObjectBytes+1))
	if copyErr == nil && written <= s.maxObjectBytes {
		copyErr = temporary.Sync()
	}
	if closeErr := temporary.Close(); closeErr != nil && copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil || written > s.maxObjectBytes {
		if written > s.maxObjectBytes {
			writeError(w, http.StatusRequestEntityTooLarge, "snapshot exceeds configured size limit")
		} else {
			writeError(w, http.StatusBadRequest, "cannot read snapshot upload")
		}
		return
	}
	if err := os.Rename(temporaryPath, destination); err != nil {
		writeError(w, http.StatusInternalServerError, "cannot publish snapshot")
		return
	}
	published = true
	if statErr == nil {
		w.WriteHeader(http.StatusNoContent)
	} else {
		w.WriteHeader(http.StatusCreated)
	}
}

func (s *server) handleSaveVaultGet(w http.ResponseWriter, r *http.Request, name string) {
	path := s.saveVaultFilePath(name)
	info, err := saveVaultRegularFileInfo(path)
	if errors.Is(err, fs.ErrNotExist) {
		writeError(w, http.StatusNotFound, "snapshot not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot open snapshot")
		return
	}
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			writeError(w, http.StatusNotFound, "snapshot not found")
		} else {
			writeError(w, http.StatusInternalServerError, "cannot open snapshot")
		}
		return
	}
	defer file.Close()
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Last-Modified", info.ModTime().UTC().Format(http.TimeFormat))
	http.ServeContent(w, r, name, info.ModTime(), file)
}

func (s *server) handleSaveVaultDelete(w http.ResponseWriter, name string) {
	if err := os.Remove(s.saveVaultFilePath(name)); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			writeError(w, http.StatusNotFound, "snapshot not found")
		} else {
			writeError(w, http.StatusInternalServerError, "cannot delete snapshot")
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleSaveVaultMove(w http.ResponseWriter, r *http.Request, sourceName string) {
	destination, err := url.Parse(strings.TrimSpace(r.Header.Get("Destination")))
	if err != nil || destination == nil || destination.RawQuery != "" || destination.Fragment != "" || destination.User != nil {
		writeError(w, http.StatusBadRequest, "invalid WebDAV MOVE destination")
		return
	}
	destinationName, err := saveVaultResourceName(destination.Path, destination.EscapedPath())
	if err != nil || destinationName == "" {
		writeError(w, http.StatusBadRequest, "invalid WebDAV MOVE destination")
		return
	}
	if !strings.HasSuffix(sourceName, ".part") || strings.HasSuffix(destinationName, ".part") {
		writeError(w, http.StatusConflict, "MOVE must publish a staged .part snapshot")
		return
	}

	source := s.saveVaultFilePath(sourceName)
	if _, err := saveVaultRegularFileInfo(source); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			writeError(w, http.StatusNotFound, "staged snapshot not found")
		} else {
			writeError(w, http.StatusInternalServerError, "cannot inspect staged snapshot")
		}
		return
	}
	target := s.saveVaultFilePath(destinationName)
	noOverwrite := strings.EqualFold(strings.TrimSpace(r.Header.Get("Overwrite")), "F")
	if noOverwrite {
		// Link creation is atomic and fails if the destination appeared between
		// our check and publication. Removing the hidden source then exposes a
		// complete, fsync'd file without ever replacing an existing snapshot.
		if err := os.Link(source, target); err != nil {
			if errors.Is(err, fs.ErrExist) {
				writeError(w, http.StatusPreconditionFailed, "destination snapshot already exists")
			} else {
				writeError(w, http.StatusInternalServerError, "cannot publish staged snapshot")
			}
			return
		}
		if err := os.Remove(source); err != nil && !errors.Is(err, fs.ErrNotExist) {
			_ = os.Remove(target)
			writeError(w, http.StatusInternalServerError, "cannot finalize staged snapshot")
			return
		}
		w.WriteHeader(http.StatusCreated)
		return
	}

	_, targetErr := os.Stat(target)
	if targetErr != nil && !errors.Is(targetErr, fs.ErrNotExist) {
		writeError(w, http.StatusInternalServerError, "cannot inspect destination snapshot")
		return
	}
	if err := os.Rename(source, target); err != nil {
		writeError(w, http.StatusInternalServerError, "cannot publish staged snapshot")
		return
	}
	if targetErr == nil {
		w.WriteHeader(http.StatusNoContent)
	} else {
		w.WriteHeader(http.StatusCreated)
	}
}

func saveVaultRegularFileInfo(path string) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("not a regular snapshot file")
	}
	return info, nil
}
