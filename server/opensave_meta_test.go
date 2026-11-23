// SPDX-FileCopyrightText: Copyright 2026 Eden Emulator Project
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

const testSwitchOdysseyName = "超级玛利欧 奥德赛"

func testCoverImage(t *testing.T, format string, width, height int) []byte {
	t.Helper()
	picture := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			picture.SetRGBA(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 0x80, A: 0xff})
		}
	}
	var buffer bytes.Buffer
	var err error
	switch format {
	case "jpeg":
		err = jpeg.Encode(&buffer, picture, &jpeg.Options{Quality: 80})
	case "png":
		err = png.Encode(&buffer, picture)
	default:
		t.Fatalf("unsupported test image format %q", format)
	}
	if err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func openSaveMetaDo(t *testing.T, request *http.Request) (*http.Response, []byte) {
	t.Helper()
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	contents, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	return response, contents
}

func decodeOpenSaveMeta(t *testing.T, body []byte) openSaveGameMeta {
	t.Helper()
	var meta openSaveGameMeta
	if err := json.Unmarshal(body, &meta); err != nil {
		t.Fatalf("decode meta %q: %v", body, err)
	}
	return meta
}

func putOpenSaveCustomName(t *testing.T, baseURL, kind, gameID, name, ifMatch string) (*http.Response, openSaveGameMeta) {
	t.Helper()
	payload, err := json.Marshal(map[string]string{"name": name})
	if err != nil {
		t.Fatal(err)
	}
	request := openSaveScopeRequest(t, http.MethodPut,
		baseURL+"/v1/opensave/scopes/"+kind+"/"+gameID+"/custom-name", payload)
	if ifMatch != "" {
		request.Header.Set("If-Match", ifMatch)
	}
	response, body := openSaveMetaDo(t, request)
	if response.StatusCode == http.StatusNoContent {
		return response, openSaveGameMeta{}
	}
	return response, decodeOpenSaveMeta(t, body)
}

func putOpenSaveCustomCover(t *testing.T, baseURL, kind, gameID string, data []byte, contentType string) (*http.Response, openSaveGameMeta) {
	t.Helper()
	request := openSaveScopeRequest(t, http.MethodPut,
		baseURL+"/v1/opensave/scopes/"+kind+"/"+gameID+"/custom-cover", data)
	request.Header.Set("Content-Type", contentType)
	response, body := openSaveMetaDo(t, request)
	if response.StatusCode == http.StatusNoContent {
		return response, openSaveGameMeta{}
	}
	return response, decodeOpenSaveMeta(t, body)
}

func deleteOpenSaveCustomCover(t *testing.T, baseURL, kind, gameID string) (*http.Response, openSaveGameMeta) {
	t.Helper()
	request := openSaveScopeRequest(t, http.MethodDelete,
		baseURL+"/v1/opensave/scopes/"+kind+"/"+gameID+"/custom-cover", nil)
	response, body := openSaveMetaDo(t, request)
	if response.StatusCode == http.StatusNoContent {
		return response, openSaveGameMeta{}
	}
	return response, decodeOpenSaveMeta(t, body)
}

func getOpenSaveCover(t *testing.T, baseURL, digest string) (*http.Response, []byte) {
	t.Helper()
	request := openSaveScopeRequest(t, http.MethodGet, baseURL+"/v1/opensave/covers/"+digest, nil)
	return openSaveMetaDo(t, request)
}

func listOpenSaveGamesForDevice(t *testing.T, baseURL, deviceID string) []openSaveGame {
	t.Helper()
	request := openSaveScopeRequest(t, http.MethodGet, baseURL+"/v1/opensave/games", nil)
	request.Header.Set("X-OpenSave-Device-ID", deviceID)
	response, body := openSaveMetaDo(t, request)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("games status = %d: %s", response.StatusCode, body)
	}
	var payload struct {
		Games []openSaveGame `json:"games"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	return payload.Games
}

func bindTestDeviceToken(t *testing.T, baseURL, deviceID string) string {
	t.Helper()
	created := createDeviceAdminToken(t, baseURL, "metadata test")
	request := deviceBearerRequest(t, http.MethodPost, baseURL+"/v1/devices/heartbeat", created.Token, nil)
	request.Header.Set("X-Eden-Device-ID", deviceID)
	response, body := deviceTestDo(t, request)
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("bind device token status = %d: %s", response.StatusCode, body)
	}
	return created.Token
}

func openSaveCoverFiles(t *testing.T, dataDir string) []string {
	t.Helper()
	root := filepath.Join(dataDir, "opensave-meta", "covers")
	files := []string{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			if os.IsNotExist(walkErr) {
				return nil
			}
			return walkErr
		}
		if !entry.IsDir() {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func snapshotDataDirectory(t *testing.T, dataDir string) map[string]time.Time {
	t.Helper()
	entries := make(map[string]time.Time)
	err := filepath.WalkDir(dataDir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		entries[path] = info.ModTime()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

func getOpenSaveScopeForTest(t *testing.T, baseURL, kind, gameID string) openSaveScopeResponse {
	t.Helper()
	request := openSaveScopeRequest(t, http.MethodGet, baseURL+"/v1/opensave/scopes/"+kind+"/"+gameID, nil)
	response, body := openSaveMetaDo(t, request)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("scope status = %d: %s", response.StatusCode, body)
	}
	var scope openSaveScopeResponse
	if err := json.Unmarshal(body, &scope); err != nil {
		t.Fatal(err)
	}
	return scope
}

func TestOpenSaveMetaNamePriority(t *testing.T) {
	_, httpServer := newAdminTestServer(t, t.TempDir())
	defer httpServer.Close()

	publishOpenSaveScope(t, httpServer.URL, "switch", "switch-0100000000010000", "a", "", []byte("switch"))
	if status, body := putOpenSaveIdentity(t, httpServer.URL, "switch", "switch-0100000000010000",
		map[string]string{"name": "客户端名称", "titleId": "0100000000010000"}); status != http.StatusOK {
		t.Fatalf("identity = %d %v", status, body)
	}
	publishOpenSaveScope(t, httpServer.URL, "pc", "client-game", "a", "", []byte("pc"))
	putOpenSaveIdentity(t, httpServer.URL, "pc", "client-game", map[string]string{"name": "客户端名称"})
	publishOpenSaveScope(t, httpServer.URL, "pc", "mystery-game", "a", "", []byte("mystery"))

	games := listOpenSaveGames(t, httpServer.URL)
	tableGame := gameByKey(games, "switch/title:0100000000010000")
	if tableGame == nil {
		t.Fatalf("missing table game: %+v", games)
	}
	if tableGame.Meta.Name != testSwitchOdysseyName || tableGame.Meta.NameSource != openSaveNameSourceTable ||
		tableGame.Meta.Rev != 0 || tableGame.Meta.Cover != nil || !tableGame.Meta.UpdatedAt.IsZero() ||
		tableGame.Meta.UpdatedBy != "" {
		t.Fatalf("table meta = %+v", tableGame.Meta)
	}
	if clientGame := gameByKey(games, "pc/scope:client-game"); clientGame == nil ||
		clientGame.Meta.Name != "客户端名称" || clientGame.Meta.NameSource != openSaveNameSourceClient {
		t.Fatalf("client game = %+v", clientGame)
	}
	if keyGame := gameByKey(games, "pc/scope:mystery-game"); keyGame == nil ||
		keyGame.Meta.Name != "mystery-game" || keyGame.Meta.NameSource != openSaveNameSourceKey {
		t.Fatalf("key game = %+v", keyGame)
	}

	response, meta := putOpenSaveCustomName(t, httpServer.URL, "switch", "switch-0100000000010000", "自定义名称", "")
	if response.StatusCode != http.StatusOK || meta.Rev != 1 || meta.Name != "自定义名称" ||
		meta.NameSource != openSaveNameSourceCustom {
		t.Fatalf("custom name = %d %+v", response.StatusCode, meta)
	}
	response, meta = putOpenSaveCustomName(t, httpServer.URL, "switch", "switch-0100000000010000", "", "")
	if response.StatusCode != http.StatusOK || meta.Rev != 2 || meta.Name != testSwitchOdysseyName ||
		meta.NameSource != openSaveNameSourceTable {
		t.Fatalf("cleared name = %d %+v", response.StatusCode, meta)
	}
	response, meta = putOpenSaveCustomName(t, httpServer.URL, "pc", "client-game", "自定义客户端名称", "")
	if response.StatusCode != http.StatusOK || meta.NameSource != openSaveNameSourceCustom {
		t.Fatalf("custom client name = %d %+v", response.StatusCode, meta)
	}
}

func TestOpenSaveCustomCoverIsSharedAcrossDevices(t *testing.T) {
	s, httpServer := newAdminTestServer(t, t.TempDir())
	defer httpServer.Close()
	publishOpenSaveScope(t, httpServer.URL, "pc", "cover-game", "a", "", []byte("cover"))

	picture := testCoverImage(t, "png", 40, 30)
	digest := sha256.Sum256(picture)
	sha := hex.EncodeToString(digest[:])
	response, meta := putOpenSaveCustomCover(t, httpServer.URL, "pc", "cover-game", picture, "image/png")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("upload cover status = %d", response.StatusCode)
	}
	if meta.Cover == nil || meta.Cover.Source != openSaveCoverSourceCustom || meta.Cover.SHA256 != sha ||
		meta.Cover.MediaType != "image/png" || meta.Cover.Width != 40 || meta.Cover.Height != 30 ||
		meta.Cover.URL != "/v1/opensave/covers/"+sha {
		t.Fatalf("cover meta = %+v", meta)
	}
	if meta.Rev != 1 || meta.UpdatedBy != "desktop-1" || meta.UpdatedAt.IsZero() {
		t.Fatalf("cover revision = %+v", meta)
	}

	// A device token bound to another device sees the same cover.
	token := bindTestDeviceToken(t, httpServer.URL, "cover-phone")
	beforeReads := snapshotDataDirectory(t, s.dataDir)
	request := deviceBearerRequest(t, http.MethodGet, httpServer.URL+"/v1/opensave/games", token, nil)
	request.Header.Set("X-OpenSave-Device-ID", "some-other-device")
	response, body := openSaveMetaDo(t, request)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("games status = %d: %s", response.StatusCode, body)
	}
	var payload struct {
		Games []openSaveGame `json:"games"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	game := gameByKey(payload.Games, "pc/scope:cover-game")
	if game == nil || game.Meta.Cover == nil || game.Meta.Cover.SHA256 != sha {
		t.Fatalf("other device game = %+v", game)
	}

	request = deviceBearerRequest(t, http.MethodGet, httpServer.URL+game.Meta.Cover.URL, token, nil)
	response, downloaded := openSaveMetaDo(t, request)
	if response.StatusCode != http.StatusOK || !bytes.Equal(downloaded, picture) {
		t.Fatalf("cover download = %d bytes=%d", response.StatusCode, len(downloaded))
	}
	if response.Header.Get("Content-Type") != "image/png" ||
		response.Header.Get("ETag") != `"`+sha+`"` ||
		response.Header.Get("Cache-Control") != "private, max-age=31536000, immutable" ||
		response.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("cover headers = %v", response.Header)
	}

	// The administrator view uses the administrator cover URL.
	response, body = adminDo(t, http.MethodGet, httpServer.URL+"/admin/api/opensave/games", nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("admin games status = %d: %s", response.StatusCode, body)
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	game = gameByKey(payload.Games, "pc/scope:cover-game")
	if game == nil || game.Meta.Cover == nil || game.Meta.Cover.URL != "/admin/api/opensave/covers/"+sha {
		t.Fatalf("admin game meta = %+v", game)
	}
	response, downloaded = adminDo(t, http.MethodGet, httpServer.URL+game.Meta.Cover.URL, nil)
	if response.StatusCode != http.StatusOK || !bytes.Equal(downloaded, picture) {
		t.Fatalf("admin cover download = %d bytes=%d", response.StatusCode, len(downloaded))
	}
	if afterReads := snapshotDataDirectory(t, s.dataDir); !reflect.DeepEqual(beforeReads, afterReads) {
		t.Fatalf("cover reads changed the data directory\nbefore: %v\nafter: %v", beforeReads, afterReads)
	}
}

func TestOpenSaveMetaIfMatchRejectsStaleRevision(t *testing.T) {
	s, httpServer := newAdminTestServer(t, t.TempDir())
	defer httpServer.Close()
	publishOpenSaveScope(t, httpServer.URL, "pc", "ifmatch-game", "a", "", []byte("ifmatch"))

	response, meta := putOpenSaveCustomName(t, httpServer.URL, "pc", "ifmatch-game", "first", "")
	if response.StatusCode != http.StatusOK || meta.Rev != 1 {
		t.Fatalf("first name = %d %+v", response.StatusCode, meta)
	}
	metaPath := filepath.Join(s.dataDir, openSaveMetaFileName)
	before, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatal(err)
	}

	response, meta = putOpenSaveCustomName(t, httpServer.URL, "pc", "ifmatch-game", "second", `"0"`)
	if response.StatusCode != http.StatusPreconditionFailed || meta.Rev != 1 || meta.Name != "first" {
		t.Fatalf("stale name = %d %+v", response.StatusCode, meta)
	}
	after, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("stale If-Match changed opensave-meta.json")
	}

	// An unquoted revision is accepted.
	response, meta = putOpenSaveCustomName(t, httpServer.URL, "pc", "ifmatch-game", "second", "1")
	if response.StatusCode != http.StatusOK || meta.Rev != 2 || meta.Name != "second" {
		t.Fatalf("unquoted If-Match = %d %+v", response.StatusCode, meta)
	}

	// Cover writes use the same precondition.
	picture := testCoverImage(t, "png", 8, 8)
	request := openSaveScopeRequest(t, http.MethodPut,
		httpServer.URL+"/v1/opensave/scopes/pc/ifmatch-game/custom-cover", picture)
	request.Header.Set("Content-Type", "image/png")
	request.Header.Set("If-Match", `"1"`)
	response, body := openSaveMetaDo(t, request)
	if response.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("stale cover status = %d: %s", response.StatusCode, body)
	}
	staleMeta := decodeOpenSaveMeta(t, body)
	if staleMeta.Rev != 2 || staleMeta.Name != "second" || staleMeta.Cover != nil {
		t.Fatalf("stale cover meta = %+v", staleMeta)
	}
	if files := openSaveCoverFiles(t, s.dataDir); len(files) != 0 {
		t.Fatalf("stale cover left files: %v", files)
	}
}

func TestOpenSaveCustomNameValidation(t *testing.T) {
	dataDir := t.TempDir()
	_, httpServer := newAdminTestServer(t, dataDir)
	defer httpServer.Close()
	publishOpenSaveScope(t, httpServer.URL, "pc", "name-game", "a", "", []byte("name"))

	for _, name := range []string{strings.Repeat("a", maxGameNameRunes+1), "bad\nname"} {
		response, _ := putOpenSaveCustomName(t, httpServer.URL, "pc", "name-game", name, "")
		if response.StatusCode != http.StatusBadRequest {
			t.Fatalf("name %q status = %d", name, response.StatusCode)
		}
	}
	if _, err := os.Stat(filepath.Join(dataDir, openSaveMetaFileName)); !os.IsNotExist(err) {
		t.Fatalf("invalid names created metadata: %v", err)
	}
	response, meta := putOpenSaveCustomName(t, httpServer.URL, "pc", "name-game",
		strings.Repeat("a", maxGameNameRunes), "")
	if response.StatusCode != http.StatusOK || len([]rune(meta.Name)) != maxGameNameRunes {
		t.Fatalf("max-length name = %d %+v", response.StatusCode, meta)
	}
	response, _ = putOpenSaveCustomName(t, httpServer.URL, "pc", "missing-name-game", "x", "")
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("missing scope status = %d", response.StatusCode)
	}
}

func TestOpenSaveCustomCoverRejectsInvalidImages(t *testing.T) {
	pngSmall := testCoverImage(t, "png", 16, 16)
	jpegSmall := testCoverImage(t, "jpeg", 16, 16)
	tests := []struct {
		name        string
		data        []byte
		contentType string
		status      int
	}{
		{name: "not an image", data: []byte("not an image"), contentType: "image/png", status: http.StatusBadRequest},
		{name: "content type mismatch", data: pngSmall, contentType: "image/jpeg", status: http.StatusBadRequest},
		{name: "jpeg as png", data: jpegSmall, contentType: "image/png", status: http.StatusBadRequest},
		{name: "too large", data: make([]byte, maxOpenSaveCustomCoverBytes+1), contentType: "image/png", status: http.StatusRequestEntityTooLarge},
		{name: "too wide", data: testCoverImage(t, "png", maxOpenSaveCoverDimension+1, 1), contentType: "image/png", status: http.StatusBadRequest},
		{name: "missing content type", data: pngSmall, contentType: "", status: http.StatusBadRequest},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dataDir := t.TempDir()
			_, httpServer := newAdminTestServer(t, dataDir)
			defer httpServer.Close()
			publishOpenSaveScope(t, httpServer.URL, "pc", "invalid-cover", "a", "", []byte("invalid"))

			request := openSaveScopeRequest(t, http.MethodPut,
				httpServer.URL+"/v1/opensave/scopes/pc/invalid-cover/custom-cover", test.data)
			if test.contentType != "" {
				request.Header.Set("Content-Type", test.contentType)
			}
			response, body := openSaveMetaDo(t, request)
			if response.StatusCode != test.status {
				t.Fatalf("status = %d, want %d: %s", response.StatusCode, test.status, body)
			}
			if _, err := os.Stat(filepath.Join(dataDir, openSaveMetaDirectoryName)); !os.IsNotExist(err) {
				t.Fatalf("invalid upload created opensave-meta: %v", err)
			}
			if _, err := os.Stat(filepath.Join(dataDir, openSaveMetaFileName)); !os.IsNotExist(err) {
				t.Fatalf("invalid upload created opensave-meta.json: %v", err)
			}
		})
	}
}

func TestOpenSaveCustomCoverCanBeCleared(t *testing.T) {
	s, httpServer := newAdminTestServer(t, t.TempDir())
	defer httpServer.Close()
	publishOpenSaveScope(t, httpServer.URL, "pc", "clear-cover", "a", "", []byte("clear"))

	picture := testCoverImage(t, "png", 20, 10)
	digest := sha256.Sum256(picture)
	sha := hex.EncodeToString(digest[:])
	response, meta := putOpenSaveCustomCover(t, httpServer.URL, "pc", "clear-cover", picture, "image/png")
	if response.StatusCode != http.StatusOK || meta.Cover == nil {
		t.Fatalf("upload = %d %+v", response.StatusCode, meta)
	}
	response, meta = deleteOpenSaveCustomCover(t, httpServer.URL, "pc", "clear-cover")
	if response.StatusCode != http.StatusOK || meta.Cover != nil || meta.Rev != 2 {
		t.Fatalf("clear = %d %+v", response.StatusCode, meta)
	}
	response, _ = getOpenSaveCover(t, httpServer.URL, sha)
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("cleared cover download = %d", response.StatusCode)
	}
	response, _ = deleteOpenSaveCustomCover(t, httpServer.URL, "pc", "clear-cover")
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("second clear = %d", response.StatusCode)
	}

	request := openSaveScopeRequest(t, http.MethodGet, httpServer.URL+"/v1/opensave/games", nil)
	response, body := openSaveMetaDo(t, request)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("games = %d", response.StatusCode)
	}
	if bytes.Contains(body, []byte(`"cover"`)) {
		t.Fatalf("cleared game still has cover: %s", body)
	}
	if files := openSaveCoverFiles(t, s.dataDir); len(files) != 1 {
		t.Fatalf("cleared cover file should remain until maintenance: %v", files)
	}

	// The metric counts referenced custom covers, not files on disk.
	response, body = adminDo(t, http.MethodGet, httpServer.URL+"/metrics", nil)
	if response.StatusCode != http.StatusOK || !bytes.Contains(body, []byte(`game_save_covers{source="custom"} 0`)) {
		t.Fatalf("metrics after clear = %d: %s", response.StatusCode, body)
	}
}

func TestOpenSaveReadOnlyEndpointsDoNotWrite(t *testing.T) {
	dataDir := t.TempDir()
	_, httpServer := newAdminTestServer(t, dataDir)
	defer httpServer.Close()
	publishOpenSaveScope(t, httpServer.URL, "pc", "readonly-game", "a", "", []byte("readonly"))

	before := snapshotDataDirectory(t, dataDir)
	request := openSaveScopeRequest(t, http.MethodGet, httpServer.URL+"/v1/opensave/games", nil)
	response, body := openSaveMetaDo(t, request)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("games = %d: %s", response.StatusCode, body)
	}
	request = openSaveScopeRequest(t, http.MethodGet, httpServer.URL+"/v1/opensave/titles/switch", nil)
	response, body = openSaveMetaDo(t, request)
	if response.StatusCode != http.StatusOK || len(body) == 0 {
		t.Fatalf("titles = %d: %s", response.StatusCode, body)
	}
	request = openSaveScopeRequest(t, http.MethodGet,
		httpServer.URL+"/v1/opensave/covers/"+strings.Repeat("a", 64), nil)
	response, _ = openSaveMetaDo(t, request)
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown cover = %d", response.StatusCode)
	}
	response, body = adminDo(t, http.MethodGet, httpServer.URL+"/admin/api/opensave/games", nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("admin games = %d: %s", response.StatusCode, body)
	}
	response, _ = adminDo(t, http.MethodGet, httpServer.URL+"/admin/api/opensave/covers/"+strings.Repeat("a", 64), nil)
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("admin unknown cover = %d", response.StatusCode)
	}

	after := snapshotDataDirectory(t, dataDir)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("read-only requests changed the data directory\nbefore: %v\nafter: %v", before, after)
	}
	if _, err := os.Stat(filepath.Join(dataDir, openSaveMetaFileName)); !os.IsNotExist(err) {
		t.Fatalf("read-only requests created opensave-meta.json: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, openSaveMetaDirectoryName)); !os.IsNotExist(err) {
		t.Fatalf("read-only requests created opensave-meta/: %v", err)
	}
}

func TestOpenSaveSharedCoverIsCollectedOnlyAfterLastReference(t *testing.T) {
	s, httpServer := newAdminTestServer(t, t.TempDir())
	defer httpServer.Close()
	publishOpenSaveScope(t, httpServer.URL, "pc", "cover-a", "a", "", []byte("a"))
	publishOpenSaveScope(t, httpServer.URL, "pc", "cover-b", "a", "", []byte("b"))

	picture := testCoverImage(t, "png", 24, 24)
	digest := sha256.Sum256(picture)
	sha := hex.EncodeToString(digest[:])
	for _, gameID := range []string{"cover-a", "cover-b"} {
		response, meta := putOpenSaveCustomCover(t, httpServer.URL, "pc", gameID, picture, "image/png")
		if response.StatusCode != http.StatusOK || meta.Cover == nil || meta.Cover.SHA256 != sha {
			t.Fatalf("upload %s = %d %+v", gameID, response.StatusCode, meta)
		}
	}
	files := openSaveCoverFiles(t, s.dataDir)
	if len(files) != 1 {
		t.Fatalf("same image stored %d files: %v", len(files), files)
	}
	coverPath := files[0]
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(coverPath, old, old); err != nil {
		t.Fatal(err)
	}
	report, err := s.runMaintenance(false)
	if err != nil {
		t.Fatal(err)
	}
	if report.Covers != 0 {
		t.Fatalf("maintenance removed a referenced cover: %d", report.Covers)
	}
	if _, err := os.Stat(coverPath); err != nil {
		t.Fatalf("referenced cover was removed: %v", err)
	}

	if response, _ := deleteOpenSaveCustomCover(t, httpServer.URL, "pc", "cover-a"); response.StatusCode != http.StatusOK {
		t.Fatalf("clear a = %d", response.StatusCode)
	}
	if _, err := os.Stat(coverPath); err != nil {
		t.Fatalf("cover removed while still referenced: %v", err)
	}
	response, downloaded := getOpenSaveCover(t, httpServer.URL, sha)
	if response.StatusCode != http.StatusOK || !bytes.Equal(downloaded, picture) {
		t.Fatalf("cover after clearing one reference = %d", response.StatusCode)
	}

	if response, _ := deleteOpenSaveCustomCover(t, httpServer.URL, "pc", "cover-b"); response.StatusCode != http.StatusOK {
		t.Fatalf("clear b = %d", response.StatusCode)
	}
	if response, _ := getOpenSaveCover(t, httpServer.URL, sha); response.StatusCode != http.StatusNotFound {
		t.Fatalf("unreferenced cover download = %d", response.StatusCode)
	}
	if _, err := os.Stat(coverPath); err != nil {
		t.Fatalf("cover removed before maintenance: %v", err)
	}

	old = time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(coverPath, old, old); err != nil {
		t.Fatal(err)
	}
	report, err = s.runMaintenance(true)
	if err != nil {
		t.Fatal(err)
	}
	if report.Covers != 1 {
		t.Fatalf("dry-run covers = %d", report.Covers)
	}
	if _, err := os.Stat(coverPath); err != nil {
		t.Fatalf("dry-run removed cover: %v", err)
	}
	report, err = s.runMaintenance(false)
	if err != nil {
		t.Fatal(err)
	}
	if report.Covers != 1 {
		t.Fatalf("maintenance covers = %d", report.Covers)
	}
	if _, err := os.Stat(coverPath); !os.IsNotExist(err) {
		t.Fatalf("maintenance did not remove cover: %v", err)
	}
}

func TestOpenSaveMetaWritesDoNotChangeScopeState(t *testing.T) {
	_, httpServer := newAdminTestServer(t, t.TempDir())
	defer httpServer.Close()
	publishOpenSaveScope(t, httpServer.URL, "pc", "stable-game", "head", "", []byte("head"))

	conflictBody := []byte("conflict")
	conflictArchive := "stable-game__main__conflict.zip"
	if status := putOpenSaveScopeArchive(t, httpServer.URL, "pc", conflictArchive, conflictBody); status != http.StatusCreated {
		t.Fatalf("conflict archive = %d", status)
	}
	conflict := openSaveScopeCandidateFor("conflict", conflictArchive, "other-parent", conflictBody, time.Now().UTC())
	response, body := putOpenSaveScopeCandidate(t, httpServer.URL,
		"/v1/opensave/scopes/pc/stable-game/conflicts/"+conflict.ID, "-", conflict)
	response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("conflict = %d: %s", response.StatusCode, body)
	}

	before := getOpenSaveScopeForTest(t, httpServer.URL, "pc", "stable-game")
	if before.Head == nil || len(before.Snapshots) != 2 || len(before.Conflicts) != 1 {
		t.Fatalf("before scope = %+v", before)
	}
	if response, _ := putOpenSaveCustomName(t, httpServer.URL, "pc", "stable-game", "稳定游戏", ""); response.StatusCode != http.StatusOK {
		t.Fatalf("custom name = %d", response.StatusCode)
	}
	picture := testCoverImage(t, "png", 10, 10)
	if response, _ := putOpenSaveCustomCover(t, httpServer.URL, "pc", "stable-game", picture, "image/png"); response.StatusCode != http.StatusOK {
		t.Fatalf("custom cover = %d", response.StatusCode)
	}
	after := getOpenSaveScopeForTest(t, httpServer.URL, "pc", "stable-game")
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("metadata writes changed scope state\nbefore: %+v\nafter: %+v", before, after)
	}
}

func TestOpenSaveMetaUpdatedByFollowsAuthentication(t *testing.T) {
	_, httpServer := newAdminTestServer(t, t.TempDir())
	defer httpServer.Close()
	publishOpenSaveScope(t, httpServer.URL, "pc", "actors-game", "a", "", []byte("actors"))

	response, meta := putOpenSaveCustomName(t, httpServer.URL, "pc", "actors-game", "shared", "")
	if response.StatusCode != http.StatusOK || meta.UpdatedBy != "desktop-1" {
		t.Fatalf("shared updatedBy = %d %+v", response.StatusCode, meta)
	}

	token := bindTestDeviceToken(t, httpServer.URL, "bound-device")
	payload, _ := json.Marshal(map[string]string{"name": "device"})
	request := deviceBearerRequest(t, http.MethodPut,
		httpServer.URL+"/v1/opensave/scopes/pc/actors-game/custom-name", token, payload)
	request.Header.Set("X-Eden-Device-ID", "impersonated-device")
	response, body := deviceTestDo(t, request)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("device name = %d: %s", response.StatusCode, body)
	}
	meta = decodeOpenSaveMeta(t, body)
	if meta.UpdatedBy != "bound-device" {
		t.Fatalf("device updatedBy = %q", meta.UpdatedBy)
	}

	response, body = adminDo(t, http.MethodPut,
		httpServer.URL+"/admin/api/opensave/games/pc/scope:actors-game/custom-name",
		map[string]string{"name": "admin"})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("admin name = %d: %s", response.StatusCode, body)
	}
	meta = decodeOpenSaveMeta(t, body)
	if meta.UpdatedBy != "admin" {
		t.Fatalf("admin updatedBy = %q", meta.UpdatedBy)
	}
}

func TestOpenSaveMetaUnchangedWritesReturnNoContent(t *testing.T) {
	s, httpServer := newAdminTestServer(t, t.TempDir())
	defer httpServer.Close()
	publishOpenSaveScope(t, httpServer.URL, "pc", "same-game", "a", "", []byte("same"))

	if response, _ := putOpenSaveCustomName(t, httpServer.URL, "pc", "same-game", "same", ""); response.StatusCode != http.StatusOK {
		t.Fatalf("first name = %d", response.StatusCode)
	}
	metaPath := filepath.Join(s.dataDir, openSaveMetaFileName)
	info, err := os.Stat(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	response, _ := putOpenSaveCustomName(t, httpServer.URL, "pc", "same-game", "same", "")
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("unchanged name = %d", response.StatusCode)
	}
	afterInfo, err := os.Stat(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(afterInfo.ModTime()) {
		t.Fatalf("unchanged name rewrote metadata: %v -> %v", info.ModTime(), afterInfo.ModTime())
	}
	games := listOpenSaveGames(t, httpServer.URL)
	if game := gameByKey(games, "pc/scope:same-game"); game == nil || game.Meta.Rev != 1 {
		t.Fatalf("rev after unchanged name = %+v", game)
	}

	picture := testCoverImage(t, "png", 12, 12)
	if response, _ := putOpenSaveCustomCover(t, httpServer.URL, "pc", "same-game", picture, "image/png"); response.StatusCode != http.StatusOK {
		t.Fatalf("first cover = %d", response.StatusCode)
	}
	files := openSaveCoverFiles(t, s.dataDir)
	if len(files) != 1 {
		t.Fatalf("cover files = %v", files)
	}
	coverInfo, err := os.Stat(files[0])
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	response, _ = putOpenSaveCustomCover(t, httpServer.URL, "pc", "same-game", picture, "image/png")
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("unchanged cover = %d", response.StatusCode)
	}
	afterCoverInfo, err := os.Stat(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if !coverInfo.ModTime().Equal(afterCoverInfo.ModTime()) {
		t.Fatalf("unchanged cover rewrote file: %v -> %v", coverInfo.ModTime(), afterCoverInfo.ModTime())
	}
	games = listOpenSaveGames(t, httpServer.URL)
	if game := gameByKey(games, "pc/scope:same-game"); game == nil || game.Meta.Rev != 2 {
		t.Fatalf("rev after unchanged cover = %+v", game)
	}

	if response, _ := deleteOpenSaveCustomCover(t, httpServer.URL, "pc", "same-game"); response.StatusCode != http.StatusOK {
		t.Fatalf("clear cover = %d", response.StatusCode)
	}
	if response, _ := deleteOpenSaveCustomCover(t, httpServer.URL, "pc", "same-game"); response.StatusCode != http.StatusNoContent {
		t.Fatalf("unchanged clear = %d", response.StatusCode)
	}
	games = listOpenSaveGames(t, httpServer.URL)
	if game := gameByKey(games, "pc/scope:same-game"); game == nil || game.Meta.Rev != 3 {
		t.Fatalf("rev after unchanged clear = %+v", game)
	}
}

func TestOpenSaveMetaCloneIsDeep(t *testing.T) {
	original := openSaveMetaState{
		Version: 1,
		Games: map[string]openSaveMetaRecord{
			"pc/scope:one": {
				Rev:         1,
				CustomName:  "one",
				CustomCover: &openSaveMetaCover{SHA256: "custom", MediaType: "image/png"},
				Upstream:    &openSaveMetaCover{SHA256: "upstream", MediaType: "image/jpeg"},
			},
		},
	}
	cloned := original.clone()
	record := cloned.Games["pc/scope:one"]
	record.CustomName = "changed"
	record.CustomCover.SHA256 = "changed-custom"
	record.Upstream.SHA256 = "changed-upstream"
	cloned.Games["pc/scope:one"] = record
	cloned.Games["pc/scope:two"] = openSaveMetaRecord{Rev: 1}

	if original.Games["pc/scope:one"].CustomName != "one" ||
		original.Games["pc/scope:one"].CustomCover.SHA256 != "custom" ||
		original.Games["pc/scope:one"].Upstream.SHA256 != "upstream" {
		t.Fatalf("clone shared record data: %+v", original.Games["pc/scope:one"])
	}
	if _, exists := original.Games["pc/scope:two"]; exists {
		t.Fatal("clone shared the games map")
	}
}

func TestOpenSaveMetaConcurrentNameWrites(t *testing.T) {
	_, httpServer := newAdminTestServer(t, t.TempDir())
	defer httpServer.Close()
	publishOpenSaveScope(t, httpServer.URL, "pc", "race-game", "a", "", []byte("race"))

	const writers = 8
	var wait sync.WaitGroup
	statuses := make([]int, writers)
	for index := 0; index < writers; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			payload, _ := json.Marshal(map[string]string{"name": fmt.Sprintf("name-%d", index)})
			request, err := http.NewRequest(http.MethodPut,
				httpServer.URL+"/v1/opensave/scopes/pc/race-game/custom-name", bytes.NewReader(payload))
			if err != nil {
				t.Errorf("writer %d request: %v", index, err)
				return
			}
			request.Header.Set("Authorization", "Bearer "+testToken)
			request.Header.Set("X-OpenSave-Device-ID", "desktop-1")
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Errorf("writer %d: %v", index, err)
				return
			}
			body, _ := io.ReadAll(response.Body)
			response.Body.Close()
			statuses[index] = response.StatusCode
			if response.StatusCode != http.StatusOK {
				t.Errorf("writer %d status = %d: %s", index, response.StatusCode, body)
			}
		}(index)
	}
	wait.Wait()

	successes := 0
	for _, status := range statuses {
		if status == http.StatusOK {
			successes++
		}
	}
	games := listOpenSaveGames(t, httpServer.URL)
	game := gameByKey(games, "pc/scope:race-game")
	if game == nil || game.Meta.Rev != successes || successes != writers {
		t.Fatalf("final rev = %+v, successes = %d", game, successes)
	}
}

func TestOpenSaveMetaAdminWritesByGameKey(t *testing.T) {
	_, httpServer := newAdminTestServer(t, t.TempDir())

	response, body := adminDo(t, http.MethodPut,
		httpServer.URL+"/admin/api/opensave/games/pc/scope:orphan/custom-name",
		map[string]string{"name": "Orphan"})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("admin orphan name = %d: %s", response.StatusCode, body)
	}
	meta := decodeOpenSaveMeta(t, body)
	if meta.Name != "Orphan" || meta.NameSource != openSaveNameSourceCustom || meta.UpdatedBy != "admin" {
		t.Fatalf("admin orphan meta = %+v", meta)
	}
	if response, body := adminDo(t, http.MethodPut,
		httpServer.URL+"/admin/api/opensave/games/not-a-key/custom-name",
		map[string]string{"name": "Bad"}); response.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid admin key = %d: %s", response.StatusCode, body)
	}

	picture := testCoverImage(t, "jpeg", 16, 12)
	digest := sha256.Sum256(picture)
	sha := hex.EncodeToString(digest[:])
	request, err := http.NewRequest(http.MethodPut,
		httpServer.URL+"/admin/api/opensave/games/switch/title:0100000000010000/custom-cover",
		bytes.NewReader(picture))
	if err != nil {
		t.Fatal(err)
	}
	request.SetBasicAuth("admin", testAdminToken)
	request.Header.Set(csrfHeader, "1")
	request.Header.Set("Content-Type", "image/jpeg")
	response, body = openSaveMetaDo(t, request)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("admin cover = %d: %s", response.StatusCode, body)
	}
	meta = decodeOpenSaveMeta(t, body)
	if meta.Cover == nil || meta.Cover.SHA256 != sha || meta.Cover.URL != "/admin/api/opensave/covers/"+sha ||
		meta.UpdatedBy != "admin" {
		t.Fatalf("admin cover meta = %+v", meta)
	}
	response, downloaded := adminDo(t, http.MethodGet, httpServer.URL+meta.Cover.URL, nil)
	if response.StatusCode != http.StatusOK || !bytes.Equal(downloaded, picture) {
		t.Fatalf("admin cover download = %d", response.StatusCode)
	}
	response, body = adminDo(t, http.MethodDelete,
		httpServer.URL+"/admin/api/opensave/games/switch/title:0100000000010000/custom-cover", nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("admin clear cover = %d: %s", response.StatusCode, body)
	}
	meta = decodeOpenSaveMeta(t, body)
	if meta.Cover != nil || meta.Rev != 2 {
		t.Fatalf("admin cleared meta = %+v", meta)
	}
}

func TestOpenSaveMetaPersistsAcrossRestart(t *testing.T) {
	dataDir := t.TempDir()
	_, httpServer := newAdminTestServer(t, dataDir)
	publishOpenSaveScope(t, httpServer.URL, "pc", "persist-game", "a", "", []byte("persist"))
	picture := testCoverImage(t, "png", 9, 7)
	response, meta := putOpenSaveCustomName(t, httpServer.URL, "pc", "persist-game", "Persisted", "")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("name = %d", response.StatusCode)
	}
	response, meta = putOpenSaveCustomCover(t, httpServer.URL, "pc", "persist-game", picture, "image/png")
	if response.StatusCode != http.StatusOK || meta.Cover == nil {
		t.Fatalf("cover = %d %+v", response.StatusCode, meta)
	}
	sha := meta.Cover.SHA256
	httpServer.Close()

	reloaded, err := newServer(dataDir, testToken, 1024*1024, testAdminToken)
	if err != nil {
		t.Fatal(err)
	}
	reloadedHTTP := httptest.NewServer(reloaded.handler())
	defer reloadedHTTP.Close()
	games := listOpenSaveGames(t, reloadedHTTP.URL)
	game := gameByKey(games, "pc/scope:persist-game")
	if game == nil || game.Meta.Name != "Persisted" || game.Meta.NameSource != openSaveNameSourceCustom ||
		game.Meta.Cover == nil || game.Meta.Cover.SHA256 != sha || game.Meta.Rev != 2 {
		t.Fatalf("reloaded game = %+v", game)
	}
}

func TestOpenSaveMetaEventIsRecorded(t *testing.T) {
	s, httpServer := newAdminTestServer(t, t.TempDir())
	defer httpServer.Close()
	publishOpenSaveScope(t, httpServer.URL, "pc", "event-game", "a", "", []byte("event"))
	if response, _ := putOpenSaveCustomName(t, httpServer.URL, "pc", "event-game", "Event", ""); response.StatusCode != http.StatusOK {
		t.Fatalf("name = %d", response.StatusCode)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	found := false
	for _, item := range s.events {
		if item.Kind == eventOpenSaveMeta && strings.Contains(item.Detail, "pc/scope:event-game") &&
			strings.Contains(item.Detail, "custom-name") {
			found = true
		}
	}
	if !found {
		t.Fatalf("metadata event missing: %+v", s.events)
	}
}
