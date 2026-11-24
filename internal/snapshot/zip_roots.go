package snapshot

import (
	"archive/zip"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jun9100/savevault/internal/delta"
	"github.com/jun9100/savevault/internal/store"
)

// RootPrefix is the directory inside a snapshot archive under which a game's
// extra save locations are stored, one folder per location name. The primary
// location's files stay at the top level, exactly where they have always
// been, so every archive ever written still restores unchanged.
//
// The leading dot is doing real work. A build that predates save locations,
// handed one of these archives, extracts the prefix as a literal folder
// inside the save directory — and a dot-prefixed directory is already
// excluded from manifests, so it is never hashed, never synced, and never
// propagates to a peer. It is inert clutter in one folder rather than a
// game's configuration files scattered through its saves.
const RootPrefix = ".savevault-locations/"

const legacyRootPrefix = ".opensave-locations/"

func splitRootEntry(entry string) (prefix, name string, ok bool) {
	for _, p := range []string{RootPrefix, legacyRootPrefix} {
		if strings.HasPrefix(entry, p) {
			rest := strings.TrimPrefix(entry, p)
			name, _, _ = strings.Cut(rest, "/")
			if name != "" {
				return p, name, true
			}
			return "", "", false
		}
	}
	return "", "", false
}

// ZipRoots archives a game's primary save location plus any extra ones.
//
// extra maps location name to this device's path for it; only mapped
// locations belong in it. A location that cannot be read is recorded in
// skipped and the archive is still written — losing the config folder from
// one snapshot is a smaller harm than having no snapshot of the save at all,
// which is what returning an error here would mean.
func ZipRoots(primary string, extra map[string]string, outPath string) (skipped []string, err error) {
	skipped, _, err = ZipRootsCapturing(primary, extra, outPath)
	return skipped, err
}

// ZipRootsCapturing is ZipRoots, also reporting what it archived: for every
// file, which save location it came from, its path within that location, and
// the hash of its contents. Recording that lets a later question — "is this
// exact content recoverable from some snapshot?" — be answered exactly,
// instead of inferred from a single whole-save hash that nothing reliably
// kept current.
func ZipRootsCapturing(primary string, extra map[string]string, outPath string) (skipped []string, captured []store.CapturedFile, err error) {
	if len(extra) == 0 {
		return ZipPathCapturing(primary, outPath)
	}

	// One archive, written in a single pass.
	//
	// Not two passes appended together: a zip has exactly one central
	// directory, at the end, and writing a second archive onto the tail of the
	// first leaves a file whose only readable index is the second one. The
	// primary save's entries are still in the bytes, and completely invisible
	// to every reader. A snapshot that silently contains no save is worse than
	// one that fails to be written.
	names := make([]string, 0, len(extra))
	for name := range extra {
		names = append(names, name)
	}
	sort.Strings(names) // stable entry order between runs

	f, err := os.Create(outPath)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	w := newSnapshotWriter(f)

	primarySkipped, primaryFiles, err := archiveInto(w, primary, "", "")
	skipped = append(skipped, primarySkipped...)
	captured = append(captured, primaryFiles...)
	if err != nil {
		w.Close()
		return skipped, captured, fmt.Errorf("archive save folder: %w", err)
	}

	for _, name := range names {
		path := extra[name]
		if strings.TrimSpace(path) == "" {
			continue
		}
		sub, subFiles, subErr := archiveInto(w, path, RootPrefix+name+"/", name)
		skipped = append(skipped, sub...)
		captured = append(captured, subFiles...)
		if subErr != nil {
			// One unreadable location must not cost the snapshot of the save.
			skipped = append(skipped, path)
		}
	}
	return skipped, captured, w.Close()
}

// archiveInto writes one directory tree into an open zip under prefix.
func archiveInto(w *zip.Writer, sourcePath, prefix, root string) (skipped []string, captured []store.CapturedFile, err error) {
	info, err := os.Stat(sourcePath)
	if err != nil {
		return nil, nil, err
	}
	if !info.IsDir() {
		name := filepath.Base(sourcePath)
		hash, addErr := addFileEntry(w, sourcePath, prefix+name)
		if addErr != nil {
			return nil, nil, addErr
		}
		return nil, []store.CapturedFile{{Root: root, Path: name, Hash: hash}}, nil
	}
	// An empty location still needs its folder recorded, or restoring would
	// not know the location was captured at all. The primary location writes
	// no such marker: its entries sit at the archive root, where an empty
	// name would be meaningless.
	if prefix != "" {
		if _, err := w.CreateHeader(&zip.FileHeader{Name: prefix, Method: zip.Store}); err != nil {
			return nil, nil, err
		}
	}

	walkErr := filepath.Walk(sourcePath, func(path string, walkInfo os.FileInfo, walkErr error) error {
		if walkErr != nil {
			if path != sourcePath {
				skipped = append(skipped, path)
				return nil
			}
			return walkErr
		}
		if path == sourcePath {
			return nil
		}
		if walkInfo.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
			return nil
		}
		rel, err := filepath.Rel(sourcePath, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if walkInfo.IsDir() {
			_, err := w.CreateHeader(&zip.FileHeader{Name: prefix + rel + "/", Method: zip.Store})
			return err
		}
		hash, addErr := addFileEntry(w, path, prefix+rel)
		if addErr != nil {
			skipped = append(skipped, path)
			return nil
		}
		captured = append(captured, store.CapturedFile{Root: root, Path: rel, Hash: hash})
		return nil
	})
	return skipped, captured, walkErr
}

// ArchivedRoots lists the extra location names stored in an archive.
func ArchivedRoots(zipPath string) ([]string, error) {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return nil, err
	}
	defer r.Close()

	seen := map[string]bool{}
	for _, f := range r.File {
		name, ok := rootOfEntry(f.Name)
		if ok {
			seen[name] = true
		}
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// rootOfEntry reports which extra location an archive entry belongs to, and
// whether it belongs to one at all. Entries outside the prefix are the
// primary location's, as they always were.
func rootOfEntry(entry string) (string, bool) {
	_, name, ok := splitRootEntry(entry)
	return name, ok
}

// UnzipRoots restores an archive, sending each location's files to that
// location's own directory on this device.
//
// A location the archive contains but this device has no path for is skipped
// and named in unplaced. It is emphatically NOT extracted into the primary
// save folder: that would put a config file among the saves, which is the
// failure this whole layout exists to avoid, and it would do so during a
// restore — the operation someone reaches for when things have already gone
// wrong.
func UnzipRoots(zipPath, primary string, extra map[string]string) (unplaced []string, err error) {
	// Every location this can write into, dropped from the hash cache. See
	// UnzipTo for why this is defence in depth rather than the guard that is
	// currently doing the work.
	defer func() {
		delta.InvalidateRoot(primary)
		for _, path := range extra {
			delta.InvalidateRoot(path)
		}
	}()

	roots, err := ArchivedRoots(zipPath)
	if err != nil {
		return nil, err
	}
	if len(roots) == 0 {
		return nil, UnzipTo(zipPath, primary)
	}

	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return nil, err
	}
	defer r.Close()

	if len(r.File) > maxArchiveEntries {
		return nil, fmt.Errorf("zip archive contains too many entries (%d > %d)", len(r.File), maxArchiveEntries)
	}

	primaryIsFile := false
	if info, statErr := os.Stat(primary); statErr == nil {
		primaryIsFile = !info.IsDir()
	} else {
		primaryIsFile = len(filepath.Ext(primary)) > 1
	}

	// Pre-validate archive structure before removing or clearing live saves.
	primaryFileCount := 0
	for _, f := range r.File {
		if f.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("zip entry %q is a symlink, which is not permitted", f.Name)
		}
		prefix, name, isRoot := splitRootEntry(f.Name)
		if !isRoot {
			if primaryIsFile {
				if f.FileInfo().IsDir() {
					return nil, fmt.Errorf("single-file save archive contains unexpected directory %q", f.Name)
				}
				primaryFileCount++
				if primaryFileCount > 1 {
					return nil, fmt.Errorf("single-file save archive contains %d primary entries, want at most 1", primaryFileCount)
				}
			} else if !delta.IsSafePath(primary, f.Name) {
				return nil, fmt.Errorf("zip entry %q escapes primary save directory", f.Name)
			}
		} else if path, ok := extra[name]; ok && strings.TrimSpace(path) != "" {
			entry := strings.TrimPrefix(f.Name, prefix+name+"/")
			if entry != "" && !delta.IsSafePath(path, entry) {
				return nil, fmt.Errorf("zip entry %q escapes extra save directory %q", f.Name, name)
			}
		}
	}

	if primaryIsFile {
		if err := os.MkdirAll(filepath.Dir(primary), 0o777); err != nil {
			return nil, err
		}
		if _, statErr := os.Stat(primary); statErr == nil {
			_ = os.Chmod(primary, 0o666)
			if err := os.Remove(primary); err != nil {
				return nil, fmt.Errorf("remove old save file: %w", err)
			}
		}
	} else {
		if err := os.MkdirAll(primary, 0o777); err != nil {
			return nil, err
		}
		if err := clearSavePathGuarded(primary); err != nil {
			return nil, fmt.Errorf("clear %s: %w", primary, err)
		}
	}

	budget := &extractBudget{remaining: maxArchiveExtractBytes}
	cleared := map[string]bool{}
	extraIsFile := map[string]bool{}
	missing := map[string]bool{}
	for _, f := range r.File {
		prefix, name, isRoot := splitRootEntry(f.Name)
		target := primary
		entry := f.Name
		if !isRoot && primaryIsFile {
			// A single-file save restores as the file itself, not into a
			// directory named after it.
			target = filepath.Dir(primary)
			entry = filepath.Base(primary)
		}
		if isRoot {
			path, ok := extra[name]
			if !ok || strings.TrimSpace(path) == "" {
				missing[name] = true
				continue
			}
			target = path
			if !cleared[name] {
				isFile := false
				if info, statErr := os.Stat(target); statErr == nil {
					isFile = !info.IsDir()
				} else {
					isFile = len(filepath.Ext(target)) > 1
				}
				extraIsFile[name] = isFile
				if isFile {
					if err := os.MkdirAll(filepath.Dir(target), 0o777); err != nil {
						return nil, err
					}
					if _, statErr := os.Stat(target); statErr == nil {
						_ = os.Chmod(target, 0o666)
						if err := os.Remove(target); err != nil {
							return nil, fmt.Errorf("remove old extra save file %s: %w", target, err)
						}
					}
				} else {
					if err := os.MkdirAll(target, 0o777); err != nil {
						return nil, err
					}
					if err := clearSavePathGuarded(target); err != nil {
						return nil, fmt.Errorf("clear %s: %w", target, err)
					}
				}
				cleared[name] = true
			}
			entry = strings.TrimPrefix(f.Name, prefix+name+"/")
			if entry == "" {
				if !extraIsFile[name] {
					if err := os.MkdirAll(target, 0o777); err != nil {
						return nil, err
					}
				}
				continue
			}
			if extraIsFile[name] {
				if f.FileInfo().IsDir() {
					return nil, fmt.Errorf("single-file extra location %q contains unexpected directory %q", name, f.Name)
				}
				target = filepath.Dir(path)
				entry = filepath.Base(path)
			}
		}
		if err := extractEntryAsLimited(f, target, entry, budget); err != nil {
			return nil, err
		}
	}

	for name := range missing {
		unplaced = append(unplaced, name)
	}
	sort.Strings(unplaced)
	return unplaced, nil
}

// RootOfArchiveEntry reports which extra save location an archive entry
// belongs to, and whether it belongs to one at all. Exported so callers that
// work on single entries — restoring one file out of a snapshot — can send it
// to the right folder instead of assuming the save folder.
func RootOfArchiveEntry(entry string) (string, bool) { return rootOfEntry(entry) }

// ArchiveEntryRelPath strips the location prefix from an entry, giving the
// path relative to that location's own folder. Entries outside the prefix are
// returned unchanged, since they are already relative to the save folder.
func ArchiveEntryRelPath(entry string) string {
	prefix, name, ok := splitRootEntry(entry)
	if !ok {
		return entry
	}
	return strings.TrimPrefix(entry, prefix+name+"/")
}
