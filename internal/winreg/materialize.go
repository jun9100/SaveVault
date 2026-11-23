package winreg

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Materialising a capture as a real file is what makes registry saves sync.
//
// A snapshot is a zip of files, and every mechanism downstream — the manifest,
// the content hash, conflict detection, the merge base — is defined over files.
// A registry key that lived only inside the archive would be invisible to all
// of them: two devices whose only difference was a registry value would hash
// identically and be judged in agreement forever, so the save would never move.
// Measured before building this: a save folder holding nothing hashes to
// e3b0c442… on every machine, which is exactly that failure.
//
// So a capture is written to a file in a directory OpenSave owns, and that
// directory is registered as one of the game's extra save locations. From
// there the existing machinery does the rest: the file is hashed, changes to it
// move the game's content hash, and it is archived into the snapshot under
// .opensave-locations/ — inside the zip, alongside the game's files.
//
// The file is deliberately NOT dot-prefixed. A dot-named entry is excluded from
// every manifest (delta.isDotEntry), which is the property that makes
// .opensave-locations/ safe for an older build to extract — and the exact
// property that would stop a registry capture from ever syncing.

// FileName is what a materialised capture is called inside its location.
const FileName = "registry.json"

// LocationName is the extra-location name a game's registry capture is
// registered under. One per game: several keys share the file.
const LocationName = "registry"

// Capture is a whole game's registry state, as written to disk.
type Snapshot struct {
	// Keys the capture was asked for, in the order they were requested, so a
	// reader can see what was looked for and not only what was found.
	Requested []string `json:"requested"`
	Keys      []Key    `json:"keys"`
	// Missing names keys that do not exist on this device. A game that has
	// never run has never written its key, and recording that is different
	// from recording that nothing was asked for.
	Missing []string `json:"missing,omitempty"`
}

// CaptureToDir captures every key into dir/registry.json.
//
// Returns the warnings a caller should surface. A key that could not be read is
// a warning rather than an error: losing one key from a snapshot is a smaller
// harm than having no snapshot, which is what returning an error here would
// mean — the same reasoning ZipRoots uses for an unreadable location.
//
// On a platform with no registry it writes nothing and warns once. That is the
// honest outcome: a Steam Deck cannot capture a Windows registry key, and
// silently writing an empty capture would let it overwrite a real one from the
// Windows device it syncs with.
func CaptureToDir(keys []string, dir string) (warnings []string, err error) {
	keys = dedupeKeys(keys)
	if len(keys) == 0 {
		return nil, nil
	}
	if !Available() {
		return []string{fmt.Sprintf(
			"%d registry key(s) could not be captured: %v", len(keys), ErrUnsupported)}, nil
	}

	snap := Snapshot{Requested: keys}
	for _, k := range keys {
		captured, found, capErr := Capture(k)
		switch {
		case capErr != nil:
			warnings = append(warnings, fmt.Sprintf("could not read registry key %s: %v", k, capErr))
		case !found:
			snap.Missing = append(snap.Missing, k)
		default:
			snap.Keys = append(snap.Keys, captured)
		}
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return warnings, fmt.Errorf("preparing the registry location: %w", err)
	}
	raw, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return warnings, err
	}
	// Written whole then renamed: a snapshot taken while this file is half
	// written would archive a truncated capture, and a truncated capture
	// restores as a partial save.
	target := filepath.Join(dir, FileName)
	tmp := target + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return warnings, err
	}
	if err := os.Rename(tmp, target); err != nil {
		os.Remove(tmp)
		return warnings, err
	}
	return warnings, nil
}

// RestoreFromDir applies a capture previously written to dir.
//
// A directory with no capture in it is not an error: a snapshot taken before
// this feature existed, or of a game with no registry keys, simply has nothing
// to put back.
func RestoreFromDir(dir string) (warnings []string, err error) {
	return restoreFromDir(dir, nil, false)
}

// RestoreFromDirAllowed applies a capture previously written to dir, restricting
// every written key and subkey to the game's locally configured allowedKeys
// allowlist (in addition to blocking dangerous system/autostart paths).
//
// An archive arriving from a remote peer or cloud server must never be able to
// redirect registry writes outside the game's configured registry paths.
func RestoreFromDirAllowed(dir string, allowedKeys []string) (warnings []string, err error) {
	return restoreFromDir(dir, allowedKeys, true)
}

func restoreFromDir(dir string, allowedKeys []string, enforceAllowlist bool) (warnings []string, err error) {
	raw, err := os.ReadFile(filepath.Join(dir, FileName))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var snap Snapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return nil, fmt.Errorf("the registry capture in this snapshot is unreadable: %w", err)
	}
	if len(snap.Keys) == 0 {
		return nil, nil
	}

	allowedCanon := make([]string, 0, len(allowedKeys))
	for _, k := range allowedKeys {
		canon := strings.ToLower(NormalizePath(k))
		if canon != "" && !isDangerousRegistryPath(canon) {
			allowedCanon = append(allowedCanon, canon)
		}
	}

	var permitted []Key
	for _, k := range snap.Keys {
		filtered, rejected := filterRestorableKey(k, allowedCanon, enforceAllowlist)
		for _, bad := range rejected {
			warnings = append(warnings, fmt.Sprintf("refused to restore disallowed registry key %s", bad))
		}
		if filtered != nil {
			permitted = append(permitted, *filtered)
		}
	}
	if len(permitted) == 0 {
		return warnings, nil
	}
	if !Available() {
		return append(warnings, fmt.Sprintf(
			"this snapshot holds %d registry key(s), which cannot be restored here: %v",
			len(permitted), ErrUnsupported)), nil
	}
	for _, k := range permitted {
		if restoreErr := Restore(k); restoreErr != nil {
			// One key failing must not abandon the others: a partial restore
			// of a save beats none of it.
			warnings = append(warnings, fmt.Sprintf("could not restore registry key %s: %v", k.Path, restoreErr))
		}
	}
	return warnings, nil
}

// filterRestorableKey validates k and all its recursive Subkeys against both
// the built-in dangerous-path denylist and (when enforceAllowlist is true)
// the game's configured registry key allowlist.
func filterRestorableKey(k Key, allowedCanon []string, enforceAllowlist bool) (*Key, []string) {
	canon := strings.ToLower(NormalizePath(k.Path))
	if canon == "" || strings.ContainsRune(canon, '\x00') || isDangerousRegistryPath(canon) {
		return nil, []string{k.Path}
	}
	if enforceAllowlist && !matchesAllowedRegistryPath(canon, allowedCanon) {
		return nil, []string{k.Path}
	}
	out := Key{
		Path:   k.Path,
		Values: k.Values,
	}
	var rejected []string
	for _, sub := range k.Subkeys {
		subCanon := strings.ToLower(NormalizePath(sub.Path))
		// Every subkey must stay strictly inside its parent key path as well
		// as passing the denylist and allowlist.
		if subCanon != canon && !strings.HasPrefix(subCanon, canon+`\`) {
			rejected = append(rejected, sub.Path)
			continue
		}
		child, childRejected := filterRestorableKey(sub, allowedCanon, enforceAllowlist)
		rejected = append(rejected, childRejected...)
		if child != nil {
			out.Subkeys = append(out.Subkeys, *child)
		}
	}
	return &out, rejected
}

func matchesAllowedRegistryPath(canon string, allowedCanon []string) bool {
	for _, allow := range allowedCanon {
		if canon == allow || strings.HasPrefix(canon, allow+`\`) {
			return true
		}
	}
	return false
}

// isDangerousRegistryPath blocks well-known Windows persistence, execution,
// and security-policy keys even if an archive or misconfiguration names them.
func isDangerousRegistryPath(canon string) bool {
	if _, sub, err := splitHive(canon); err != nil || strings.TrimSpace(sub) == "" {
		return true
	}
	dangerousFragments := []string{
		`\currentversion\run`,
		`\currentversion\runonce`,
		`\currentversion\runservices`,
		`\currentversion\policies\explorer\run`,
		`\currentversion\winlogon`,
		`\currentversion\image file execution options`,
		`\command processor`,
		`\shell\open\command`,
		`\windows nt\currentversion\windows`,
		`\control\session manager`,
		`\environment`,
	}
	for _, frag := range dangerousFragments {
		if strings.Contains(canon, frag) {
			return true
		}
	}
	return false
}

// dedupeKeys collapses spellings of one key and orders them, so two devices
// capture the same save in the same shape and the file's hash agrees.
func dedupeKeys(keys []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		canon := strings.ToLower(NormalizePath(k))
		if seen[canon] {
			continue
		}
		seen[canon] = true
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
