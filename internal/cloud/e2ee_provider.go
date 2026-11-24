package cloud

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/chacha20poly1305"
)

const (
	cloudE2EEMagic     = "SVE2E1"
	cloudE2EESaltLen   = 16
	cloudE2EENonceLen  = chacha20poly1305.NonceSizeX // 24 bytes
	cloudE2EEHeaderLen = len(cloudE2EEMagic) + cloudE2EESaltLen + cloudE2EENonceLen + 4
)

// Tunable vars so unit tests can exercise multi-chunk streaming and Argon2id
// without spending seconds on production parameters.
var (
	cloudE2EEChunkSize    uint32 = 64 * 1024
	cloudE2EEArgonTime    uint32 = 2
	cloudE2EEArgonMemory  uint32 = 64 * 1024 // 64 MiB
	cloudE2EEArgonThreads uint8  = 2
)

// encryptedProvider transparently encrypts uploaded snapshot/head archives with
// Argon2id + XChaCha20-Poly1305 chunked AEAD before sending them to any
// third-party storage provider, and decrypts them back on download.
// Legacy unencrypted archives without the SVE2E1 header pass through untouched
// on download so existing cloud backups remain restorable.
type encryptedProvider struct {
	inner      provider
	passphrase string
}

func (e encryptedProvider) upload(f *os.File, _ int64, fileName string) error {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	tmp, err := os.CreateTemp("", "savevault-e2ee-up-*")
	if err != nil {
		return fmt.Errorf("e2ee: create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		tmp.Close()
		_ = os.Remove(tmpPath)
	}()

	if err := encryptCloudStream(tmp, f, e.passphrase); err != nil {
		return err
	}
	info, err := tmp.Stat()
	if err != nil {
		return err
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return err
	}
	return e.inner.upload(tmp, info.Size(), fileName)
}

func (e encryptedProvider) list() ([]CloudFile, error) {
	return e.inner.list()
}

func (e encryptedProvider) download(fileName, localPath string) error {
	if err := os.MkdirAll(filepath.Dir(localPath), 0o777); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(localPath), ".savevault-e2ee-dl-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpPath)

	if err := e.inner.download(fileName, tmpPath); err != nil {
		return err
	}

	rawFile, err := os.Open(tmpPath)
	if err != nil {
		return err
	}
	magic := make([]byte, len(cloudE2EEMagic))
	n, readErr := io.ReadFull(rawFile, magic)
	if readErr != nil && !errors.Is(readErr, io.EOF) && !errors.Is(readErr, io.ErrUnexpectedEOF) {
		rawFile.Close()
		return readErr
	}
	isEncrypted := n == len(cloudE2EEMagic) && string(magic) == cloudE2EEMagic
	if !isEncrypted {
		// Backward-compatible passthrough for legacy plaintext backups.
		rawFile.Close()
		return os.Rename(tmpPath, localPath)
	}
	if _, err := rawFile.Seek(0, io.SeekStart); err != nil {
		rawFile.Close()
		return err
	}
	err = writeFileWhole(localPath, func(w io.Writer) error {
		return decryptCloudStream(w, rawFile, e.passphrase)
	})
	rawFile.Close()
	return err
}

func (e encryptedProvider) remove(f CloudFile) error {
	return e.inner.remove(f)
}

func deriveCloudE2EEKey(passphrase string, salt []byte) []byte {
	return argon2.IDKey(
		[]byte(passphrase),
		salt,
		cloudE2EEArgonTime,
		cloudE2EEArgonMemory,
		cloudE2EEArgonThreads,
		chacha20poly1305.KeySize,
	)
}

func chunkNonce(baseNonce []byte, chunkIndex uint64) []byte {
	n := make([]byte, cloudE2EENonceLen)
	copy(n, baseNonce)
	var idxBytes [8]byte
	binary.BigEndian.PutUint64(idxBytes[:], chunkIndex)
	for i := 0; i < 8; i++ {
		n[cloudE2EENonceLen-8+i] ^= idxBytes[i]
	}
	return n
}

func chunkAAD(chunkIndex uint64, final bool) []byte {
	var aad [9]byte
	binary.BigEndian.PutUint64(aad[:8], chunkIndex)
	if final {
		aad[8] = 1
	}
	return aad[:]
}

func encryptCloudStream(dst io.Writer, src io.Reader, passphrase string) error {
	if passphrase == "" {
		return errors.New("e2ee: passphrase is required")
	}
	salt := make([]byte, cloudE2EESaltLen)
	if _, err := rand.Read(salt); err != nil {
		return err
	}
	baseNonce := make([]byte, cloudE2EENonceLen)
	if _, err := rand.Read(baseNonce); err != nil {
		return err
	}
	key := deriveCloudE2EEKey(passphrase, salt)
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return err
	}

	chunkSize := cloudE2EEChunkSize
	if chunkSize == 0 {
		chunkSize = 64 * 1024
	}
	var hdr [cloudE2EEHeaderLen]byte
	copy(hdr[0:6], cloudE2EEMagic)
	copy(hdr[6:22], salt)
	copy(hdr[22:46], baseNonce)
	binary.BigEndian.PutUint32(hdr[46:50], chunkSize)
	if _, err := dst.Write(hdr[:]); err != nil {
		return err
	}

	buf := make([]byte, int(chunkSize)+1)
	var chunkIndex uint64
	var lenBuf [4]byte

	// Read chunkSize+1 bytes each step so we always know with certainty whether
	// the current chunk is the final one (preventing truncation attacks).
	pending := 0
	for {
		n, rerr := io.ReadFull(src, buf[pending:])
		total := pending + n
		eof := errors.Is(rerr, io.EOF) || errors.Is(rerr, io.ErrUnexpectedEOF)
		if rerr != nil && !eof {
			return rerr
		}
		if eof {
			ct := aead.Seal(nil, chunkNonce(baseNonce, chunkIndex), buf[:total], chunkAAD(chunkIndex, true))
			binary.BigEndian.PutUint32(lenBuf[:], uint32(len(ct)))
			if _, err := dst.Write(lenBuf[:]); err != nil {
				return err
			}
			if _, err := dst.Write(ct); err != nil {
				return err
			}
			return nil
		}
		// Full chunkSize bytes + 1 lookahead byte read -> not final.
		ct := aead.Seal(nil, chunkNonce(baseNonce, chunkIndex), buf[:chunkSize], chunkAAD(chunkIndex, false))
		binary.BigEndian.PutUint32(lenBuf[:], uint32(len(ct)))
		if _, err := dst.Write(lenBuf[:]); err != nil {
			return err
		}
		if _, err := dst.Write(ct); err != nil {
			return err
		}
		buf[0] = buf[chunkSize]
		pending = 1
		chunkIndex++
	}
}

func decryptCloudStream(dst io.Writer, src io.Reader, passphrase string) error {
	if passphrase == "" {
		return errors.New("e2ee: passphrase is required to decrypt cloud archive")
	}
	var hdr [cloudE2EEHeaderLen]byte
	if _, err := io.ReadFull(src, hdr[:]); err != nil {
		return fmt.Errorf("e2ee: truncated encrypted archive header: %w", err)
	}
	if !bytes.Equal(hdr[0:6], []byte(cloudE2EEMagic)) {
		return errors.New("e2ee: invalid encrypted archive header")
	}
	salt := hdr[6:22]
	baseNonce := hdr[22:46]
	chunkSize := binary.BigEndian.Uint32(hdr[46:50])
	if chunkSize == 0 || chunkSize > 16<<20 {
		return errors.New("e2ee: invalid chunk size in archive header")
	}

	key := deriveCloudE2EEKey(passphrase, salt)
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return err
	}

	maxCipherChunk := uint64(chunkSize) + uint64(aead.Overhead())
	var lenBuf [4]byte
	var chunkIndex uint64
	for {
		_, err := io.ReadFull(src, lenBuf[:])
		if err != nil {
			return fmt.Errorf("e2ee: unexpected end of encrypted stream: %w", err)
		}
		ctLen := binary.BigEndian.Uint32(lenBuf[:])
		if uint64(ctLen) < uint64(aead.Overhead()) || uint64(ctLen) > maxCipherChunk {
			return errors.New("e2ee: invalid encrypted chunk length")
		}
		ct := make([]byte, ctLen)
		if _, err := io.ReadFull(src, ct); err != nil {
			return fmt.Errorf("e2ee: truncated encrypted chunk: %w", err)
		}

		// Try non-final AAD first; if that fails, try final AAD (ensures the
		// stream ends immediately after the authenticated final chunk).
		nonce := chunkNonce(baseNonce, chunkIndex)
		plain, openErr := aead.Open(nil, nonce, ct, chunkAAD(chunkIndex, false))
		if openErr == nil {
			if _, err := dst.Write(plain); err != nil {
				return err
			}
			chunkIndex++
			continue
		}
		plain, finalErr := aead.Open(nil, nonce, ct, chunkAAD(chunkIndex, true))
		if finalErr != nil {
			return errors.New("e2ee: decryption failed (wrong passphrase or corrupted archive)")
		}
		if _, err := dst.Write(plain); err != nil {
			return err
		}
		// Reject trailing garbage after the authenticated final chunk.
		var extra [1]byte
		if n, _ := src.Read(extra[:]); n > 0 {
			return errors.New("e2ee: unexpected trailing bytes after final chunk")
		}
		return nil
	}
}
