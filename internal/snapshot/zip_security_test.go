package snapshot

import (
	"archive/zip"
	"os"
	"path/filepath"
	"testing"
)

func TestParseExportEntryNameRejectsPathTraversal(t *testing.T) {
	bad := []string{
		"../evil__main__snap_1.zip",
		"game__../evil__snap_1.zip",
		"game__main__../snap_1.zip",
		`game__main__sub\file.zip`,
		"C:__main__snap_1.zip",
		"game__main__snap_1\x00.zip",
	}
	for _, name := range bad {
		if _, _, _, ok := ParseExportEntryName(name); ok {
			t.Errorf("ParseExportEntryName(%q) = ok, want false", name)
		}
	}
	if g, b, s, ok := ParseExportEntryName("hollow-knight__main__snap_123.zip"); !ok || g != "hollow-knight" || b != "main" || s != "snap_123" {
		t.Fatalf("valid name rejected: (%q, %q, %q, %v)", g, b, s, ok)
	}
}

func TestUnzipToRejectsSymlinksAndTraversal(t *testing.T) {
	makeZip := func(t *testing.T, name string, mode os.FileMode) string {
		t.Helper()
		zipPath := filepath.Join(t.TempDir(), "test.zip")
		f, err := os.Create(zipPath)
		if err != nil {
			t.Fatal(err)
		}
		zw := zip.NewWriter(f)
		hdr := &zip.FileHeader{Name: name, Method: zip.Store}
		hdr.SetMode(mode)
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte("payload"))
		_ = zw.Close()
		_ = f.Close()
		return zipPath
	}

	cases := []struct {
		label string
		name  string
		mode  os.FileMode
	}{
		{"symlink", "link.sav", os.ModeSymlink | 0o777},
		{"backslash-traversal", `..\..\outside.sav`, 0o644},
		{"drive-prefix", `C:/Windows/win.ini`, 0o644},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			z := makeZip(t, tc.name, tc.mode)
			dest := filepath.Join(t.TempDir(), "save")
			if err := UnzipTo(z, dest); err == nil {
				t.Fatalf("UnzipTo(%s) succeeded, want error", tc.label)
			}
		})
	}
}
