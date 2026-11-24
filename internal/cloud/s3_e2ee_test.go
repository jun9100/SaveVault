package cloud

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/jun9100/savevault/internal/store"
)

func TestS3ProviderSigV4UploadListPaginationDownloadDelete(t *testing.T) {
	var mu sync.Mutex
	objects := map[string][]byte{}
	authHeaders := []string{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 Credential=AKIA_TEST/") ||
			r.Header.Get("X-Amz-Date") == "" ||
			r.Header.Get("X-Amz-Content-Sha256") == "" {
			http.Error(w, "missing sigv4 headers", http.StatusForbidden)
			return
		}
		mu.Lock()
		authHeaders = append(authHeaders, auth)
		mu.Unlock()

		const bucketPrefix = "/my-bucket"
		if !strings.HasPrefix(r.URL.Path, bucketPrefix) {
			http.Error(w, "wrong bucket path "+r.URL.Path, http.StatusNotFound)
			return
		}
		key := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, bucketPrefix), "/")

		switch r.Method {
		case http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			sum := sha256.Sum256(body)
			if got := r.Header.Get("X-Amz-Content-Sha256"); got != hex.EncodeToString(sum[:]) {
				http.Error(w, "sha256 mismatch", http.StatusBadRequest)
				return
			}
			mu.Lock()
			objects[key] = body
			mu.Unlock()
			w.WriteHeader(http.StatusOK)

		case http.MethodGet:
			if r.URL.Query().Get("list-type") == "2" {
				prefix := r.URL.Query().Get("prefix")
				token := r.URL.Query().Get("continuation-token")
				mu.Lock()
				keys := make([]string, 0, len(objects))
				for k := range objects {
					if strings.HasPrefix(k, prefix) {
						keys = append(keys, k)
					}
				}
				sort.Strings(keys)
				// Force 2 pages when >= 2 items exist to exercise pagination.
				start := 0
				if token == "page2" && len(keys) > 1 {
					start = 1
				}
				end := len(keys)
				truncated := false
				nextToken := ""
				if start == 0 && len(keys) > 1 {
					end = 1
					truncated = true
					nextToken = "page2"
				}
				var b strings.Builder
				b.WriteString(`<?xml version="1.0" encoding="UTF-8"?><ListBucketResult>`)
				for _, k := range keys[start:end] {
					fmt.Fprintf(&b, `<Contents><Key>%s</Key><LastModified>2026-11-24T06:00:00.000Z</LastModified><Size>%d</Size></Contents>`, k, len(objects[k]))
				}
				fmt.Fprintf(&b, `<IsTruncated>%v</IsTruncated>`, truncated)
				if nextToken != "" {
					fmt.Fprintf(&b, `<NextContinuationToken>%s</NextContinuationToken>`, nextToken)
				}
				b.WriteString(`</ListBucketResult>`)
				mu.Unlock()
				w.Header().Set("Content-Type", "application/xml")
				_, _ = w.Write([]byte(b.String()))
				return
			}
			mu.Lock()
			data, ok := objects[key]
			mu.Unlock()
			if !ok {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write(data)

		case http.MethodDelete:
			mu.Lock()
			delete(objects, key)
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)

		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}))
	defer srv.Close()

	svc, st := newTestService(t)
	svc.HTTP = srv.Client()

	cfg := store.CloudConfig{
		Enabled:     true,
		Provider:    "s3",
		URL:         srv.URL,
		S3Bucket:    "my-bucket",
		S3Region:    "auto",
		S3Prefix:    "saves/pc",
		S3PathStyle: true,
		Username:    "AKIA_TEST",
		Password:    "SECRET_TEST_KEY",
	}
	if !Ready(cfg) {
		t.Fatal("expected S3 config to be Ready")
	}
	if err := st.UpdateCloudConfig(cfg); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	f1 := filepath.Join(dir, "hades__main__snap_1.zip")
	f2 := filepath.Join(dir, "celeste__main__snap_2.zip")
	if err := os.WriteFile(f1, []byte("PK\x03\x04hades-payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f2, []byte("PK\x03\x04celeste-payload"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := svc.Upload(f1, "hades__main__snap_1.zip"); err != nil {
		t.Fatalf("upload 1: %v", err)
	}
	if err := svc.Upload(f2, "celeste__main__snap_2.zip"); err != nil {
		t.Fatalf("upload 2: %v", err)
	}

	listed, err := svc.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != 2 {
		t.Fatalf("listed len = %d, want 2 (%+v)", len(listed), listed)
	}

	dl := filepath.Join(dir, "downloaded.zip")
	if err := svc.Download("hades__main__snap_1.zip", dl); err != nil {
		t.Fatalf("download: %v", err)
	}
	got, err := os.ReadFile(dl)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "PK\x03\x04hades-payload" {
		t.Fatalf("downloaded bytes = %q", got)
	}

	if err := svc.Delete(listed[0]); err != nil {
		t.Fatalf("delete: %v", err)
	}
	afterDelete, err := svc.List()
	if err != nil {
		t.Fatalf("list after delete: %v", err)
	}
	if len(afterDelete) != 1 {
		t.Fatalf("after delete len = %d, want 1", len(afterDelete))
	}

	// Verify virtual-hosted URL construction when S3PathStyle is false on a non-IP host.
	vh := s3Provider{cfg: store.CloudConfig{URL: "https://s3.us-east-1.amazonaws.com", S3Bucket: "vault-bucket"}}
	u, err := vh.endpointURL("prefix/a.zip", nil)
	if err != nil {
		t.Fatal(err)
	}
	if u.String() != "https://vault-bucket.s3.us-east-1.amazonaws.com/prefix/a.zip" {
		t.Fatalf("virtual-hosted url = %q", u.String())
	}
}

func TestCloudE2EERoundTripBoundariesTamperAndLegacyPassthrough(t *testing.T) {
	// Use small chunk size and fast Argon2id settings during tests.
	origChunk, origTime, origMem, origThreads := cloudE2EEChunkSize, cloudE2EEArgonTime, cloudE2EEArgonMemory, cloudE2EEArgonThreads
	cloudE2EEChunkSize = 1024
	cloudE2EEArgonTime = 1
	cloudE2EEArgonMemory = 8 * 1024
	cloudE2EEArgonThreads = 1
	t.Cleanup(func() {
		cloudE2EEChunkSize, cloudE2EEArgonTime, cloudE2EEArgonMemory, cloudE2EEArgonThreads = origChunk, origTime, origMem, origThreads
	})

	remoteDir := t.TempDir()
	workDir := t.TempDir()
	svc, st := newTestService(t)

	if err := st.UpdateCloudConfig(store.CloudConfig{
		Enabled:        true,
		Provider:       "local",
		URL:            remoteDir,
		E2EEEnabled:    true,
		E2EEPassphrase: "correct horse battery staple",
	}); err != nil {
		t.Fatal(err)
	}

	// Test every chunk boundary: empty, chunk-1, chunk, chunk+1, 2*chunk, 2*chunk+17.
	sizes := []int{0, 1023, 1024, 1025, 2048, 2065}
	for _, sz := range sizes {
		payload := make([]byte, sz)
		for i := range payload {
			payload[i] = byte((i*31 + 7) & 0xff)
		}
		if sz >= 4 {
			copy(payload[:4], []byte("PK\x03\x04"))
		}
		name := fmt.Sprintf("game__main__snap_%d.zip", sz)
		srcPath := filepath.Join(workDir, "src_"+name)
		if err := os.WriteFile(srcPath, payload, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := svc.Upload(srcPath, name); err != nil {
			t.Fatalf("upload sz=%d: %v", sz, err)
		}

		// Remote bytes must start with SVE2E1 and must never leak raw plaintext ZIP header.
		remoteRaw, err := os.ReadFile(filepath.Join(remoteDir, name))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.HasPrefix(remoteRaw, []byte(cloudE2EEMagic)) {
			t.Fatalf("sz=%d remote file missing SVE2E1 magic", sz)
		}
		if sz >= 32 && bytes.Contains(remoteRaw, payload[:32]) {
			t.Fatalf("sz=%d plaintext leaked into remote ciphertext", sz)
		}

		dstPath := filepath.Join(workDir, "dst_"+name)
		if err := svc.Download(name, dstPath); err != nil {
			t.Fatalf("download sz=%d: %v", sz, err)
		}
		decrypted, err := os.ReadFile(dstPath)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(decrypted, payload) {
			t.Fatalf("sz=%d decrypted mismatch (got %d bytes, want %d)", sz, len(decrypted), len(payload))
		}
	}

	// Wrong passphrase must fail decryption cleanly.
	cfg, _ := st.GetCloudConfig()
	cfg.E2EEPassphrase = "wrong passphrase"
	if err := st.UpdateCloudConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if err := svc.Download("game__main__snap_2065.zip", filepath.Join(workDir, "wrong.zip")); err == nil {
		t.Fatal("expected decryption failure with wrong passphrase")
	}

	// Restore correct passphrase and verify 1-byte ciphertext tamper fails.
	cfg.E2EEPassphrase = "correct horse battery staple"
	if err := st.UpdateCloudConfig(cfg); err != nil {
		t.Fatal(err)
	}
	tamperFile := filepath.Join(remoteDir, "game__main__snap_2065.zip")
	raw, _ := os.ReadFile(tamperFile)
	raw[len(raw)-1] ^= 0xff
	if err := os.WriteFile(tamperFile, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := svc.Download("game__main__snap_2065.zip", filepath.Join(workDir, "tampered.zip")); err == nil {
		t.Fatal("expected tampered ciphertext to fail authentication")
	}

	// Legacy unencrypted ZIP in remote folder must download transparently.
	legacyBytes := []byte("PK\x03\x04legacy-plaintext-archive")
	if err := os.WriteFile(filepath.Join(remoteDir, "legacy__main__snap_1.zip"), legacyBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	legacyDst := filepath.Join(workDir, "legacy-out.zip")
	if err := svc.Download("legacy__main__snap_1.zip", legacyDst); err != nil {
		t.Fatalf("legacy passthrough download failed: %v", err)
	}
	gotLegacy, _ := os.ReadFile(legacyDst)
	if !bytes.Equal(gotLegacy, legacyBytes) {
		t.Fatalf("legacy passthrough bytes = %q", gotLegacy)
	}
}
