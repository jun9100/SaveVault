package cloud

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

// dropbox keeps snapshots in /SaveVault of the signed-in account's Dropbox.
type dropbox struct {
	s *Service
}

func (p dropbox) token() (string, error) {
	return p.s.getOrRefreshAccessToken("dropbox")
}

func (p dropbox) upload(f *os.File, size int64, fileName string) error {
	token, err := p.token()
	if err != nil {
		return err
	}
	if size > dropboxSessionThreshold {
		err = p.uploadSession(token, fileName, f, size)
	} else {
		err = p.uploadSimple(token, fileName, f, size)
	}
	if err != nil {
		return fmt.Errorf("Dropbox: %w", err)
	}
	return nil
}

// uploadSimple streams one request (≤150 MB per Dropbox's API).
func (p dropbox) uploadSimple(token, fileName string, f *os.File, size int64) error {
	args, _ := json.Marshal(map[string]any{"path": "/SaveVault/" + fileName, "mode": "overwrite", "mute": true})
	req, err := http.NewRequest(http.MethodPost, p.s.Endpoints.DropboxContent+"/2/files/upload", f)
	if err != nil {
		return err
	}
	req.ContentLength = size
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Dropbox-API-Arg", string(args))
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := p.s.doTransfer(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return transferOK(resp)
}

// uploadSession uses upload sessions for big files: start, append chunks,
// finish with the commit.
func (p dropbox) uploadSession(token, fileName string, f *os.File, size int64) error {
	call := func(path string, arg any, body io.Reader, bodyLen int64) (map[string]any, error) {
		argRaw, _ := json.Marshal(arg)
		req, err := http.NewRequest(http.MethodPost, p.s.Endpoints.DropboxContent+path, body)
		if err != nil {
			return nil, err
		}
		req.ContentLength = bodyLen
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Dropbox-API-Arg", string(argRaw))
		req.Header.Set("Content-Type", "application/octet-stream")
		resp, err := p.s.doTransfer(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if err := transferOK(resp); err != nil {
			return nil, err
		}
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return out, nil
	}

	first := dropboxChunkSize
	if size < first {
		first = size
	}
	out, err := call("/2/files/upload_session/start", map[string]any{"close": false},
		io.NewSectionReader(f, 0, first), first)
	if err != nil {
		return fmt.Errorf("session start: %w", err)
	}
	sessionID, _ := out["session_id"].(string)
	if sessionID == "" {
		return fmt.Errorf("session start: no session_id")
	}

	offset := first
	for offset < size {
		n := dropboxChunkSize
		if remaining := size - offset; remaining < n {
			n = remaining
		}
		_, err := call("/2/files/upload_session/append_v2", map[string]any{
			"cursor": map[string]any{"session_id": sessionID, "offset": offset},
			"close":  false,
		}, io.NewSectionReader(f, offset, n), n)
		if err != nil {
			return fmt.Errorf("session append at %d: %w", offset, err)
		}
		offset += n
	}

	_, err = call("/2/files/upload_session/finish", map[string]any{
		"cursor": map[string]any{"session_id": sessionID, "offset": offset},
		"commit": map[string]any{"path": "/SaveVault/" + fileName, "mode": "overwrite", "mute": true},
	}, nil, 0)
	if err != nil {
		return fmt.Errorf("session finish: %w", err)
	}
	return nil
}

func (p dropbox) list() ([]CloudFile, error) {
	token, err := p.token()
	if err != nil {
		return nil, err
	}
	files, missing, err := p.listFolder(token, "/SaveVault", false)
	if err != nil {
		return nil, err
	}
	if missing {
		// Smooth upgrade: read pre-rebrand backups from /OpenSave if /SaveVault
		// has not been created yet.
		legacyFiles, legacyMissing, legacyErr := p.listFolder(token, "/OpenSave", true)
		if legacyErr == nil && !legacyMissing {
			return legacyFiles, nil
		}
		return []CloudFile{}, nil
	}
	return files, nil
}

func (p dropbox) listFolder(token, folder string, tagLegacyID bool) ([]CloudFile, bool, error) {
	// Following the cursor while Dropbox says there is more: one call is one
	// page, and a page is not the folder.
	endpoint := "/2/files/list_folder"
	body, _ := json.Marshal(map[string]string{"path": folder})
	var files []CloudFile
	for page := 0; page < maxListPages; page++ {
		req, _ := http.NewRequest(http.MethodPost, p.s.Endpoints.DropboxAPI+endpoint, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")

		var out struct {
			Entries []struct {
				Tag            string `json:".tag"`
				Name           string `json:"name"`
				Size           int64  `json:"size"`
				ClientModified string `json:"client_modified"`
			} `json:"entries"`
			Cursor  string `json:"cursor"`
			HasMore bool   `json:"has_more"`
		}
		missing, err := p.listPage(req, &out)
		if err != nil {
			return nil, false, err
		}
		if missing && page == 0 {
			return []CloudFile{}, true, nil
		}
		if missing {
			// Mid-listing, a conflict is a cursor Dropbox has reset, not an
			// empty folder.
			return nil, false, fmt.Errorf("Dropbox: the listing was reset part-way through; try again")
		}
		for _, e := range out.Entries {
			if e.Tag == "file" && strings.HasSuffix(e.Name, ".zip") {
				cf := CloudFile{Name: e.Name, SizeBytes: e.Size, CreatedTime: e.ClientModified}
				if tagLegacyID {
					cf.ID = folder + "/" + e.Name
				}
				files = append(files, cf)
			}
		}
		if !out.HasMore {
			return files, false, nil
		}
		if out.Cursor == "" {
			return nil, false, errListTooLong("Dropbox")
		}
		endpoint = "/2/files/list_folder/continue"
		body, _ = json.Marshal(map[string]string{"cursor": out.Cursor})
	}
	return nil, false, errListTooLong("Dropbox")
}

// listPage runs one list_folder or list_folder/continue call. missing
// reports the SaveVault folder not existing yet, which Dropbox answers with a
// conflict and which is an empty listing, not an error.
func (p dropbox) listPage(req *http.Request, out any) (missing bool, err error) {
	resp, err := p.s.httpClient().Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusConflict {
		return true, nil
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return false, fmt.Errorf("Dropbox: HTTP %d - %s", resp.StatusCode, raw)
	}
	return false, json.NewDecoder(resp.Body).Decode(out)
}

func (p dropbox) download(fileName, localPath string) error {
	token, err := p.token()
	if err != nil {
		return err
	}
	path := "/SaveVault/" + fileName
	if strings.HasPrefix(fileName, "/OpenSave/") || strings.HasPrefix(fileName, "/SaveVault/") {
		path = fileName
	}
	tryDownload := func(target string) error {
		args, _ := json.Marshal(map[string]string{"path": target})
		req, _ := http.NewRequest(http.MethodPost, p.s.Endpoints.DropboxContent+"/2/files/download", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Dropbox-API-Arg", string(args))
		return p.s.fetchToFile(req, localPath)
	}
	if err := tryDownload(path); err != nil {
		if path == "/SaveVault/"+fileName && strings.Contains(err.Error(), "HTTP 409") {
			if legacyErr := tryDownload("/OpenSave/" + fileName); legacyErr == nil {
				return nil
			}
		}
		return fmt.Errorf("Dropbox: %w", err)
	}
	return nil
}

func (p dropbox) remove(f CloudFile) error {
	token, err := p.token()
	if err != nil {
		return err
	}
	path := "/SaveVault/" + f.Name
	if strings.HasPrefix(f.ID, "/OpenSave/") {
		path = f.ID
	}
	body, _ := json.Marshal(map[string]string{"path": path})
	req, _ := http.NewRequest(http.MethodPost, p.s.Endpoints.DropboxAPI+"/2/files/delete_v2", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	if err := p.s.doOK(req); err != nil {
		return fmt.Errorf("Dropbox: %w", err)
	}
	return nil
}
