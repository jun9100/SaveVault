// SPDX-FileCopyrightText: Copyright 2026 Eden Emulator Project
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"testing"
)

func p2AdminReq(t *testing.T, method, url string, body io.Reader, contentType string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.SetBasicAuth("admin", "admin-secret")
	if method != http.MethodGet && method != http.MethodHead {
		req.Header.Set("X-Game-Save-Admin", "1")
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return req
}

func p2SyncReq(t *testing.T, method, url string, body io.Reader, contentType string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer sync-token")
	req.Header.Set(edenSaveSyncProtocolHeader, "2")
	req.Header.Set("X-SaveVault-Device", "dev-p2")
	req.Header.Set("X-SaveVault-Device-Name", "Deck-P2")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return req
}

func TestP2QuotaLedgerO1AccuracyAndReconcile(t *testing.T) {
	srv, err := newServerWithOptions(t.TempDir(), "sync-token", 16<<20, serverOptions{
		adminToken:        "admin-secret",
		saveVaultUsername: "davuser",
		saveVaultPassword: "davpassword",
		quotaBytes:        1024 * 1024,
	})
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	defer srv.Close()
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	if got, want := srv.usedBytes(), vaultDirectoryUsedBytes(srv.dataDir); got != want {
		t.Fatalf("initial ledger=%d want=%d", got, want)
	}

	// 1. Upload an encrypted blob.
	blob := bytes.Repeat([]byte("A"), 4096)
	blobSum := sha256.Sum256(blob)
	blobID := hex.EncodeToString(blobSum[:])
	resp, err := http.DefaultClient.Do(p2SyncReq(t, http.MethodPut, ts.URL+"/v1/blobs/"+blobID, bytes.NewReader(blob), "application/octet-stream"))
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("put blob: err=%v status=%v", err, resp)
	}
	resp.Body.Close()

	if got, want := srv.usedBytes(), vaultDirectoryUsedBytes(srv.dataDir); got != want {
		t.Fatalf("after blob ledger=%d want=%d", got, want)
	}

	// 2. Upload a native SaveVault archive.
	archive := bytes.Repeat([]byte("Z"), 8192)
	archURL := ts.URL + "/v1/savevault/files/switch/01007EF00011E000__user__snap_1700000001000.zip"
	resp, err = http.DefaultClient.Do(p2SyncReq(t, http.MethodPut, archURL, bytes.NewReader(archive), "application/zip"))
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("put archive: err=%v status=%v", err, resp)
	}
	resp.Body.Close()

	if got, want := srv.usedBytes(), vaultDirectoryUsedBytes(srv.dataDir); got != want {
		t.Fatalf("after archive ledger=%d want=%d", got, want)
	}

	// 3. Delete the archive and verify ledger drops cleanly in O(1).
	resp, err = http.DefaultClient.Do(p2SyncReq(t, http.MethodDelete, archURL, nil, ""))
	if err != nil || resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete archive: err=%v status=%v", err, resp)
	}
	resp.Body.Close()

	if got, want := srv.usedBytes(), vaultDirectoryUsedBytes(srv.dataDir); got != want {
		t.Fatalf("after delete ledger=%d want=%d", got, want)
	}

	// 4. Admin quota reconcile endpoint works and increments reconcile counter.
	beforeReconciles := srv.usedBytesReconciles.Load()
	resp, err = http.DefaultClient.Do(p2AdminReq(t, http.MethodPost, ts.URL+"/admin/api/quota/reconcile", nil, ""))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("reconcile: err=%v status=%v", err, resp)
	}
	var rec map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&rec); err != nil {
		t.Fatalf("decode reconcile: %v", err)
	}
	resp.Body.Close()
	if int64(rec["used_bytes"].(float64)) != vaultDirectoryUsedBytes(srv.dataDir) {
		t.Fatalf("reconciled used_bytes mismatch: %v", rec)
	}
	if srv.usedBytesReconciles.Load() <= beforeReconciles {
		t.Fatalf("expected reconcile counter to advance")
	}
}

func TestP2FastCDCChunkingDedupAndAssemble(t *testing.T) {
	// Deterministic 512 KiB pseudo-random payload simulating a binary game save.
	rng := rand.New(rand.NewSource(42))
	base := make([]byte, 512*1024)
	rng.Read(base)

	chunksA := splitFastCDC(base)
	if len(chunksA) < 3 {
		t.Fatalf("expected multiple FastCDC chunks for 512 KiB buffer, got %d", len(chunksA))
	}
	// Reassemble locally and verify exact byte identity.
	var rebuilt bytes.Buffer
	setA := make(map[string]bool, len(chunksA))
	for _, ch := range chunksA {
		rebuilt.Write(ch.Data)
		setA[ch.SHA256] = true
		if ch.Length > fastCDCMaxSize {
			t.Fatalf("chunk exceeded max size: %d", ch.Length)
		}
	}
	if !bytes.Equal(rebuilt.Bytes(), base) {
		t.Fatal("FastCDC chunks did not reassemble identically")
	}

	// Shift-resilience check: prepend 137 bytes at offset 0 and verify most chunks deduplicate.
	shifted := append(bytes.Repeat([]byte{0x5A}, 137), base...)
	chunksB := splitFastCDC(shifted)
	shared := 0
	for _, ch := range chunksB {
		if setA[ch.SHA256] {
			shared++
		}
	}
	if shared*2 < len(chunksB) {
		t.Fatalf("expected >50%% chunk reuse after front insertion, got %d/%d", shared, len(chunksB))
	}

	// End-to-end HTTP chunk upload, missing query, tamper rejection, assemble, and legacy GET.
	srv, err := newServerWithOptions(t.TempDir(), "sync-token", 16<<20, serverOptions{
		adminToken: "admin-secret",
	})
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	defer srv.Close()
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	allHashes := make([]string, len(chunksA))
	for i, ch := range chunksA {
		allHashes[i] = ch.SHA256
	}

	// 1. Check missing chunks before upload.
	qBody, _ := json.Marshal(map[string]any{"hashes": allHashes})
	resp, err := http.DefaultClient.Do(p2SyncReq(t, http.MethodPost, ts.URL+"/v1/savevault/chunks/missing", bytes.NewReader(qBody), "application/json"))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("missing query: err=%v resp=%v", err, resp)
	}
	var missRes struct {
		Missing []string `json:"missing"`
		Present []string `json:"present"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&missRes)
	resp.Body.Close()
	if len(missRes.Missing) != len(setA) || len(missRes.Present) != 0 {
		t.Fatalf("unexpected initial missing=%d present=%d", len(missRes.Missing), len(missRes.Present))
	}

	// 2. Tampered chunk upload must be rejected with 422.
	badData := append([]byte(nil), chunksA[0].Data...)
	badData[0] ^= 0xFF
	resp, err = http.DefaultClient.Do(p2SyncReq(t, http.MethodPut, ts.URL+"/v1/savevault/chunks/"+chunksA[0].SHA256, bytes.NewReader(badData), "application/octet-stream"))
	if err != nil || resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 for tampered chunk, got err=%v status=%v", err, resp)
	}
	resp.Body.Close()

	// 3. Upload all chunks (and confirm duplicate upload returns 200 exists).
	for _, ch := range chunksA {
		resp, err = http.DefaultClient.Do(p2SyncReq(t, http.MethodPut, ts.URL+"/v1/savevault/chunks/"+ch.SHA256, bytes.NewReader(ch.Data), "application/octet-stream"))
		if err != nil || (resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK) {
			t.Fatalf("put chunk %s: err=%v status=%v", ch.SHA256, err, resp)
		}
		resp.Body.Close()
	}
	resp, err = http.DefaultClient.Do(p2SyncReq(t, http.MethodPut, ts.URL+"/v1/savevault/chunks/"+chunksA[0].SHA256, bytes.NewReader(chunksA[0].Data), "application/octet-stream"))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 exists on duplicate chunk, got err=%v status=%v", err, resp)
	}
	resp.Body.Close()

	// 4. GET chunk returns exact bytes with immutable cache header.
	resp, err = http.DefaultClient.Do(p2SyncReq(t, http.MethodGet, ts.URL+"/v1/savevault/chunks/"+chunksA[0].SHA256, nil, ""))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("get chunk: err=%v status=%v", err, resp)
	}
	gotChunk, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.Equal(gotChunk, chunksA[0].Data) {
		t.Fatal("GET chunk returned mismatched bytes")
	}

	// 5. Assemble immutable SaveVault archive from chunks and download via standard legacy archive GET.
	wholeSum := sha256.Sum256(base)
	wholeHex := hex.EncodeToString(wholeSum[:])
	asmBody, _ := json.Marshal(map[string]any{
		"chunks":          allHashes,
		"expected_sha256": wholeHex,
	})
	archName := "01007EF00011E000__user__snap_1700000009000.zip"
	resp, err = http.DefaultClient.Do(p2SyncReq(t, http.MethodPost, ts.URL+"/v1/savevault/chunks/assemble/switch/"+archName, bytes.NewReader(asmBody), "application/json"))
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("assemble archive: err=%v status=%v", err, resp)
	}
	resp.Body.Close()

	resp, err = http.DefaultClient.Do(p2SyncReq(t, http.MethodGet, ts.URL+"/v1/savevault/files/switch/"+archName, nil, ""))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("download assembled archive: err=%v status=%v", err, resp)
	}
	gotArchive, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.Equal(gotArchive, base) {
		t.Fatal("assembled archive content does not match original payload")
	}

	// 6. Admin stats and immediate GC clean up chunks while leaving the assembled archive intact.
	resp, err = http.DefaultClient.Do(p2AdminReq(t, http.MethodGet, ts.URL+"/admin/api/chunks/stats", nil, ""))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("chunk stats: err=%v status=%v", err, resp)
	}
	var stats struct {
		Chunks int   `json:"chunks"`
		Bytes  int64 `json:"bytes"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&stats)
	resp.Body.Close()
	if stats.Chunks != len(setA) || stats.Bytes <= 0 {
		t.Fatalf("unexpected chunk stats: %+v", stats)
	}

	gcBody := bytes.NewBufferString(`{"min_age_seconds": 0}`)
	resp, err = http.DefaultClient.Do(p2AdminReq(t, http.MethodPost, ts.URL+"/admin/api/chunks/gc", gcBody, "application/json"))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("chunk gc: err=%v status=%v", err, resp)
	}
	resp.Body.Close()

	if count, _ := srv.chunkStoreStats(); count != 0 {
		t.Fatalf("expected 0 chunks after min_age_seconds=0 gc, got %d", count)
	}
	if got, want := srv.usedBytes(), vaultDirectoryUsedBytes(srv.dataDir); got != want {
		t.Fatalf("ledger after gc=%d want=%d", got, want)
	}
}

func TestP2VaultExportDryRunImportAndRestore(t *testing.T) {
	srcSrv, err := newServerWithOptions(t.TempDir(), "sync-token", 16<<20, serverOptions{
		adminToken: "admin-secret",
		vaultID:    "source-nas",
	})
	if err != nil {
		t.Fatalf("new src server: %v", err)
	}
	defer srcSrv.Close()
	srcTS := httptest.NewServer(srcSrv.handler())
	defer srcTS.Close()

	// Populate archive, profile metadata, and webhook channel on source server.
	archivePayload := []byte("PK\x03\x04savevault-migration-payload-v2")
	archPath := "/v1/savevault/files/switch/01007EF00011E000__user__snap_1700000012000.zip"
	resp, err := http.DefaultClient.Do(p2SyncReq(t, http.MethodPut, srcTS.URL+archPath, bytes.NewReader(archivePayload), "application/zip"))
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("put archive on src: err=%v status=%v", err, resp)
	}
	resp.Body.Close()

	metaBody := bytes.NewBufferString(`{"custom_titles":{"switch:01007EF00011E000":"旷野之息"}}`)
	resp, err = http.DefaultClient.Do(p2SyncReq(t, http.MethodPut, srcTS.URL+"/v1/savevault/profile/metadata", metaBody, "application/json"))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("put profile metadata: err=%v status=%v", err, resp)
	}
	resp.Body.Close()

	whBody := bytes.NewBufferString(`{"channels":[{"name":"Ops","type":"generic","url":"https://example.invalid/hook","events":["conflict"],"enabled":true}]}`)
	resp, err = http.DefaultClient.Do(p2AdminReq(t, http.MethodPut, srcTS.URL+"/admin/api/webhooks", whBody, "application/json"))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("put webhooks: err=%v status=%v", err, resp)
	}
	resp.Body.Close()

	// Export entire vault as .tar.gz.
	resp, err = http.DefaultClient.Do(p2AdminReq(t, http.MethodGet, srcTS.URL+"/admin/api/vault/export", nil, ""))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("export vault: err=%v status=%v", err, resp)
	}
	bundleBytes, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if len(bundleBytes) == 0 {
		t.Fatal("empty export bundle")
	}

	// Spin up fresh destination server.
	dstSrv, err := newServerWithOptions(t.TempDir(), "sync-token", 16<<20, serverOptions{
		adminToken: "admin-secret",
		vaultID:    "target-vps",
	})
	if err != nil {
		t.Fatalf("new dst server: %v", err)
	}
	defer dstSrv.Close()
	dstTS := httptest.NewServer(dstSrv.handler())
	defer dstTS.Close()

	// 1. Dry-run import validates without touching target vault.
	resp, err = http.DefaultClient.Do(p2AdminReq(t, http.MethodPost, dstTS.URL+"/admin/api/vault/import?dryRun=true", bytes.NewReader(bundleBytes), "application/gzip"))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("dry-run import: err=%v status=%v", err, resp)
	}
	var dryRes map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&dryRes)
	resp.Body.Close()
	if dryRes["valid"] != true || dryRes["source_vault_id"] != "source-nas" {
		t.Fatalf("unexpected dry-run result: %+v", dryRes)
	}
	if dstSrv.usedBytes() != 0 {
		t.Fatalf("dry-run should not write to dst vault, usedBytes=%d", dstSrv.usedBytes())
	}

	// 2. Tampered archive (corrupted file bytes against manifest) must fail with 422.
	tampered := tamperTarGzEntry(t, bundleBytes, "profile-metadata.json", []byte(`{"version":1,"revision":99}`))
	resp, err = http.DefaultClient.Do(p2AdminReq(t, http.MethodPost, dstTS.URL+"/admin/api/vault/import?dryRun=true", bytes.NewReader(tampered), "application/gzip"))
	if err != nil || resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 on checksum mismatch, got err=%v status=%v", err, resp)
	}
	resp.Body.Close()

	// 3. Real import restores archive, profile metadata, webhooks, and O(1) quota ledger!
	resp, err = http.DefaultClient.Do(p2AdminReq(t, http.MethodPost, dstTS.URL+"/admin/api/vault/import", bytes.NewReader(bundleBytes), "application/gzip"))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("apply import: err=%v status=%v", err, resp)
	}
	resp.Body.Close()

	resp, err = http.DefaultClient.Do(p2SyncReq(t, http.MethodGet, dstTS.URL+archPath, nil, ""))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("get migrated archive: err=%v status=%v", err, resp)
	}
	gotArchive, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.Equal(gotArchive, archivePayload) {
		t.Fatalf("migrated archive mismatch: got=%q", gotArchive)
	}

	resp, err = http.DefaultClient.Do(p2SyncReq(t, http.MethodGet, dstTS.URL+"/v1/savevault/profile/metadata", nil, ""))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("get migrated profile metadata: err=%v status=%v", err, resp)
	}
	var gotMeta profileMetadataState
	_ = json.NewDecoder(resp.Body).Decode(&gotMeta)
	resp.Body.Close()
	if gotMeta.CustomTitles["switch:01007EF00011E000"] != "旷野之息" {
		t.Fatalf("migrated custom title missing: %+v", gotMeta)
	}

	resp, err = http.DefaultClient.Do(p2AdminReq(t, http.MethodGet, dstTS.URL+"/admin/api/webhooks", nil, ""))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("get migrated webhooks: err=%v status=%v", err, resp)
	}
	var whRes struct {
		Channels []webhookChannelView `json:"channels"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&whRes)
	resp.Body.Close()
	if len(whRes.Channels) != 1 || whRes.Channels[0].Name != "Ops" {
		t.Fatalf("migrated webhooks missing: %+v", whRes)
	}

	if got, want := dstSrv.usedBytes(), vaultDirectoryUsedBytes(dstSrv.dataDir); got != want || got == 0 {
		t.Fatalf("post-import ledger=%d want=%d", got, want)
	}
}

func tamperTarGzEntry(t *testing.T, archive []byte, target string, replacement []byte) []byte {
	t.Helper()
	gzIn, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatalf("open gzip: %v", err)
	}
	defer gzIn.Close()
	tr := tar.NewReader(gzIn)

	var out bytes.Buffer
	gzOut := gzip.NewWriter(&out)
	tw := tar.NewWriter(gzOut)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read tar: %v", err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("read tar entry: %v", err)
		}
		if hdr.Name == target {
			data = replacement
			hdr.Size = int64(len(data))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write tar header: %v", err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatalf("write tar data: %v", err)
		}
	}
	_ = tw.Close()
	_ = gzOut.Close()
	return out.Bytes()
}
