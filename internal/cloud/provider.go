package cloud

import (
	"os"

	"github.com/jun9100/savevault/internal/store"
)

// provider is one kind of place snapshots are kept. Each lives in its own
// file (provider_*.go); this is all the Service knows of them. They used to
// be six cases of one switch in each of Upload, List, Download and Delete —
// four switches that had to agree, in a file where a change to Dropbox meant
// reading past Google Drive, OneDrive and WebDAV to find it.
type provider interface {
	// upload stores the open file under fileName, replacing any file of that
	// name. size is f's length.
	upload(f *os.File, size int64, fileName string) error
	// list returns every snapshot zip stored, not one page of them.
	list() ([]CloudFile, error)
	// download writes the stored fileName to localPath.
	download(fileName, localPath string) error
	// remove deletes one stored file.
	remove(f CloudFile) error
}

// providerFor returns the provider cfg names, or false for one this build
// does not know. When client-side E2EE is enabled for a third-party provider,
// the returned provider transparently wraps uploads and downloads with
// Argon2id + XChaCha20-Poly1305 chunked AEAD.
func (s *Service) providerFor(cfg store.CloudConfig) (provider, bool) {
	var base provider
	switch cfg.Provider {
	case "local":
		base = localFolder{cfg: cfg}
	case "webdav":
		base = webDAV{s: s, cfg: cfg}
	case "s3":
		base = s3Provider{s: s, cfg: cfg}
	case GameSaveProvider:
		normalized, err := NormalizeGameSaveURL(cfg.URL)
		if err != nil {
			return failedProvider{err: err}, true
		}
		cfg.URL = normalized
		return gameSave{s: s, cfg: cfg}, true
	case "webhook":
		return webhook{s: s, cfg: cfg}, true
	case "google_drive":
		base = googleDrive{s: s, cfg: cfg}
	case "dropbox":
		base = dropbox{s: s}
	case "onedrive":
		base = oneDrive{s: s}
	default:
		return nil, false
	}
	if cfg.E2EEEnabled && cfg.E2EEPassphrase != "" {
		return encryptedProvider{inner: base, passphrase: cfg.E2EEPassphrase}, true
	}
	return base, true
}

// failedProvider keeps a malformed built-in provider distinct from an
// unsupported provider. Callers can surface its concrete setup error instead
// of quietly treating a malformed built-in provider as unsupported.
type failedProvider struct {
	err error
}

func (p failedProvider) upload(*os.File, int64, string) error { return p.err }
func (p failedProvider) list() ([]CloudFile, error)           { return nil, p.err }
func (p failedProvider) download(string, string) error        { return p.err }
func (p failedProvider) remove(CloudFile) error               { return p.err }
