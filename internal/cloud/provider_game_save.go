package cloud

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path"
	"runtime"
	"strings"
	"time"

	"github.com/jun9100/savevault/internal/snapshot"
	"github.com/jun9100/savevault/internal/store"
	"github.com/jun9100/savevault/internal/version"
)

// NormalizeGameSaveURL validates a SaveVault cloud service address. Native
// endpoints live below /v1/savevault and are appended by the client.
func NormalizeGameSaveURL(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed == nil {
		return "", fmt.Errorf("invalid game-save service address")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("game-save service address must use http or https")
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("game-save service address must include a host")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("game-save service address cannot include credentials, a query, or a fragment")
	}
	escapedPath := strings.ToLower(parsed.EscapedPath())
	if strings.Contains(escapedPath, "%2f") || strings.Contains(escapedPath, "%5c") {
		return "", fmt.Errorf("game-save service address cannot contain encoded path separators")
	}
	basePath := strings.TrimRight(parsed.Path, "/")
	for _, segment := range strings.Split(strings.Trim(basePath, "/"), "/") {
		if segment == "." || segment == ".." {
			return "", fmt.Errorf("game-save service address cannot contain relative path segments")
		}
	}
	// Accept the service root as well as copied native or WebDAV endpoint URLs
	// (including legacy /v1/opensave or /opensave paths during upgrades).
	lowerPath := strings.ToLower(basePath)
	switch {
	case strings.HasSuffix(lowerPath, "/v1/savevault"):
		basePath = basePath[:len(basePath)-len("/v1/savevault")]
	case strings.HasSuffix(lowerPath, "/v1/opensave"):
		basePath = basePath[:len(basePath)-len("/v1/opensave")]
	case strings.EqualFold(path.Base(basePath), "savevault"), strings.EqualFold(path.Base(basePath), "opensave"):
		basePath = path.Dir(basePath)
	}
	parsed.Path = strings.TrimRight(basePath, "/")
	parsed.RawPath = ""
	return strings.TrimRight(parsed.String(), "/"), nil
}

type gameSave struct {
	s   *Service
	cfg store.CloudConfig
}

func (p gameSave) token() string { return strings.TrimSpace(p.cfg.Password) }

func (p gameSave) fileType(name string) string {
	gameID, _, _, ok := snapshot.ParseExportEntryName(name)
	if !ok {
		gameID, _, _, ok = ParseHeadFileName(name)
	}
	if !ok {
		return "pc"
	}
	game, err := p.s.Store.GetGame(gameID)
	if err != nil {
		if canonicalID, linked := p.s.Store.ResolveGameAlias(gameID); linked {
			game, err = p.s.Store.GetGame(canonicalID)
		}
	}
	if err != nil || game.SaveType == "" {
		return "pc"
	}
	return game.SaveType
}

func (p gameSave) endpoint(kind, name string) string {
	return strings.TrimRight(p.cfg.URL, "/") + "/v1/savevault/files/" + url.PathEscape(kind) + "/" + url.PathEscape(name)
}

// authorizeReadOnly intentionally omits device headers. A native archive or
// scope GET is inspection, not synchronization; omitting those headers also
// keeps the read harmless against an older service that still records device
// activity in its broad token middleware.
func (p gameSave) authorizeReadOnly(req *http.Request) error {
	if p.token() == "" {
		return fmt.Errorf("game-save sync key is required")
	}
	req.Header.Set("Authorization", "Bearer "+p.token())
	return nil
}

func (p gameSave) authorize(req *http.Request) error {
	settings, err := p.s.Store.GetSettings()
	if err != nil {
		return fmt.Errorf("read SaveVault device identity: %w", err)
	}
	deviceID := strings.TrimSpace(settings.NodeID)
	deviceName := strings.TrimSpace(settings.DeviceName)
	if deviceName == "" {
		deviceName = "SaveVault " + runtime.GOOS
	}
	platform := "savevault-" + runtime.GOOS
	encodedDeviceName := encodeDeviceNameHeader(deviceName)
	req.Header.Set("Authorization", "Bearer "+p.token())
	for _, header := range []string{"X-SaveVault", "X-Eden"} {
		req.Header.Set(header+"-Device-ID", deviceID)
		req.Header.Set(header+"-Device-Name", encodedDeviceName)
		req.Header.Set(header+"-Platform", platform)
		req.Header.Set(header+"-Version", version.Version)
	}
	return nil
}

// encodeDeviceNameHeader sends printable ASCII names verbatim and RFC 8187
// encodes non-ASCII names ("UTF-8''...") so HTTP header transport preserves
// Unicode device names cleanly (matching game-save's decodeDeviceHeader).
func encodeDeviceNameHeader(name string) string {
	needsEncoding := false
	for i := 0; i < len(name); i++ {
		if name[i] < 0x20 || name[i] >= 0x7f {
			needsEncoding = true
			break
		}
	}
	if !needsEncoding {
		return name
	}
	return "UTF-8''" + url.PathEscape(name)
}

func (p gameSave) upload(f *os.File, size int64, fileName string) error {
	if p.token() == "" {
		return fmt.Errorf("game-save sync key is required")
	}
	req, err := http.NewRequest(http.MethodPut, p.endpoint(p.fileType(fileName), fileName), f)
	if err != nil {
		return err
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", "application/zip")
	if err := p.authorize(req); err != nil {
		return err
	}
	resp, err := p.transfer(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if gone, ok := parseGameSaveScopeGone(resp); ok {
		return gone
	}
	return transferOK(resp)
}

func (p gameSave) list() ([]CloudFile, error) {
	if p.token() == "" {
		return nil, fmt.Errorf("game-save sync key is required")
	}
	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(p.cfg.URL, "/")+"/v1/savevault/files", nil)
	if err != nil {
		return nil, err
	}
	if err := p.authorizeReadOnly(req); err != nil {
		return nil, err
	}
	resp, err := p.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if err := transferOK(resp); err != nil {
		return nil, err
	}
	var body struct {
		Files []struct {
			Type        string    `json:"type"`
			Name        string    `json:"name"`
			SizeBytes   int64     `json:"sizeBytes"`
			CreatedTime time.Time `json:"createdTime"`
		} `json:"files"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	files := make([]CloudFile, 0, len(body.Files))
	for _, item := range body.Files {
		files = append(files, CloudFile{ID: item.Type, Name: item.Name, SizeBytes: item.SizeBytes, CreatedTime: item.CreatedTime.UTC().Format(time.RFC3339)})
	}
	return files, nil
}

func (p gameSave) download(fileName, localPath string) error {
	return p.downloadFile(CloudFile{Name: fileName}, localPath)
}

func (p gameSave) downloadFile(file CloudFile, localPath string) error {
	kind := strings.TrimSpace(file.ID)
	if kind == "" {
		kind = p.fileType(file.Name)
	}
	req, err := http.NewRequest(http.MethodGet, p.endpoint(kind, file.Name), nil)
	if err != nil {
		return err
	}
	if err := p.authorizeReadOnly(req); err != nil {
		return err
	}
	return p.fetchToFile(req, localPath)
}

func (p gameSave) remove(f CloudFile) error {
	kind := f.ID
	if kind == "" {
		kind = p.fileType(f.Name)
	}
	req, err := http.NewRequest(http.MethodDelete, p.endpoint(kind, f.Name), nil)
	if err != nil {
		return err
	}
	if err := p.authorize(req); err != nil {
		return err
	}
	resp, err := p.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return transferOK(resp)
}
