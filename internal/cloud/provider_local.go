package cloud

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jun9100/savevault/internal/store"
)

// localFolder keeps snapshots in a folder on this machine or a mounted share
// (a NAS, a synced folder another program uploads).
type localFolder struct {
	cfg store.CloudConfig
}

func (p localFolder) upload(f *os.File, size int64, fileName string) error {
	return p.put(f, fileName)
}

func (p localFolder) resolveFile(fileName string) (string, error) {
	if p.cfg.URL == "" {
		return "", fmt.Errorf("no local folder destination configured")
	}
	if fileName == "" || fileName == "." || fileName == ".." || strings.ContainsAny(fileName, `/\`+"\x00") {
		return "", fmt.Errorf("invalid cloud file name %q", fileName)
	}
	base, err := filepath.Abs(p.cfg.URL)
	if err != nil {
		return "", err
	}
	target := filepath.Join(base, fileName)
	rel, err := filepath.Rel(base, target)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("cloud file name %q escapes destination folder", fileName)
	}
	return target, nil
}

// put stores what r holds under fileName, appearing under that name only
// once it is all there (see writeFileWhole). Another device reading the same
// folder never lists a snapshot that is still being copied, and a copy that
// fails leaves nothing that looks like one.
func (p localFolder) put(r io.Reader, fileName string) error {
	target, err := p.resolveFile(fileName)
	if err != nil {
		return err
	}
	return writeFileWhole(target, func(w io.Writer) error {
		_, err := io.Copy(w, r)
		return err
	})
}

func (p localFolder) list() ([]CloudFile, error) {
	if p.cfg.URL == "" {
		return []CloudFile{}, nil
	}
	entries, err := os.ReadDir(p.cfg.URL)
	if err != nil {
		return []CloudFile{}, nil
	}
	var files []CloudFile
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".zip") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, CloudFile{
			Name: e.Name(), SizeBytes: info.Size(),
			CreatedTime: info.ModTime().UTC().Format(time.RFC3339),
		})
	}
	return files, nil
}

func (p localFolder) download(fileName, localPath string) error {
	target, err := p.resolveFile(fileName)
	if err != nil {
		return err
	}
	src, err := os.Open(target)
	if err != nil {
		return fmt.Errorf("file %q not found in local folder", fileName)
	}
	defer src.Close()
	return writeFileWhole(localPath, func(w io.Writer) error {
		_, err := io.Copy(w, src)
		return err
	})
}

func (p localFolder) remove(f CloudFile) error {
	target, err := p.resolveFile(f.Name)
	if err != nil {
		return err
	}
	return os.Remove(target)
}
