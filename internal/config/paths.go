// Package config resolves SaveVault's on-disk locations and performs the
// one-time ".opensave" / ".savesync" -> ".savevault" home directory migration
// on startup so existing installations upgrade smoothly.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

const (
	legacySavesyncDirName = ".savesync"
	legacyOpenSaveDirName = ".opensave"
	homeDirName           = ".savevault"

	// LegacyDBFileName is the JSON database file imported on first SQLite run.
	LegacyDBFileName = "savevault-db.json"

	// SQLiteFileName is the embedded SQLite database file.
	SQLiteFileName = "savevault.db"

	backupsDirName = "backups"
)

// Paths holds every filesystem location SaveVault needs, resolved once at
// startup relative to the user's home directory.
type Paths struct {
	HomeDir      string
	LegacyDB     string
	SQLiteDB     string
	BackupsDir   string
	MigrationLog string
	AppCacheFile string // Steam AppID->name cache
}

// Resolve migrates a legacy ~/.opensave (or ~/.savesync) directory to
// ~/.savevault if needed, renames legacy database files inside it, ensures the
// home directory exists, and returns the resolved Paths.
func Resolve() (Paths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Paths{}, fmt.Errorf("resolve home dir: %w", err)
	}
	return resolveWithin(home)
}

// ResolveAt uses dataDir directly as the SaveVault home directory (tests
// and portable installs) — no legacy home-directory migration is attempted.
func ResolveAt(dataDir string) (Paths, error) {
	return buildPaths(dataDir)
}

func resolveWithin(home string) (Paths, error) {
	newHome := filepath.Join(home, homeDirName)

	if !dirExists(newHome) {
		for _, legacyName := range []string{legacyOpenSaveDirName, legacySavesyncDirName} {
			oldHome := filepath.Join(home, legacyName)
			if !dirExists(oldHome) {
				continue
			}
			if err := os.Rename(oldHome, newHome); err != nil {
				return Paths{}, fmt.Errorf("migrate %s to %s: %w", oldHome, newHome, err)
			}
			break
		}
	}

	if dirExists(newHome) {
		migrateLegacyDatabaseFiles(newHome)
	}

	return buildPaths(newHome)
}

func migrateLegacyDatabaseFiles(homeDir string) {
	// Migrate SQLite database (and active WAL/SHM sidecars if present).
	newSQLite := filepath.Join(homeDir, SQLiteFileName)
	oldSQLite := filepath.Join(homeDir, "opensave.db")
	if fileExists(oldSQLite) && !fileExists(newSQLite) {
		if err := os.Rename(oldSQLite, newSQLite); err == nil {
			for _, suffix := range []string{"-wal", "-shm"} {
				if fileExists(oldSQLite+suffix) && !fileExists(newSQLite+suffix) {
					_ = os.Rename(oldSQLite+suffix, newSQLite+suffix)
				}
			}
		}
	}

	// Migrate legacy JSON database file if present.
	newLegacyDB := filepath.Join(homeDir, LegacyDBFileName)
	if !fileExists(newLegacyDB) {
		for _, oldName := range []string{"opensave-db.json", "savesync-db.json"} {
			oldPath := filepath.Join(homeDir, oldName)
			if fileExists(oldPath) {
				_ = os.Rename(oldPath, newLegacyDB)
				break
			}
		}
	}
}

func buildPaths(homeDir string) (Paths, error) {
	// Private to this user. The database in here holds the device's private
	// key — the one that decrypts every save sent to it and proves who it is
	// to every peer — plus the Google tokens and the vault keys. With the
	// usual umask, 0o777 became a world-readable directory and SQLite's
	// default made a world-readable file inside it, so on a shared Linux or
	// macOS machine any other account could read all of that. Windows was
	// never exposed: the profile folder carries its own per-user ACL.
	//
	// 0o700 is what ssh and gpg use for the same reason, and an existing
	// directory is tightened too, because the ones already out there are
	// the ones that matter.
	if err := os.MkdirAll(homeDir, 0o700); err != nil {
		return Paths{}, fmt.Errorf("create home dir: %w", err)
	}
	tightenPermissions(homeDir)
	backupsDir := filepath.Join(homeDir, backupsDirName)
	if err := os.MkdirAll(backupsDir, 0o700); err != nil {
		return Paths{}, fmt.Errorf("create backups dir: %w", err)
	}

	return Paths{
		HomeDir:      homeDir,
		LegacyDB:     filepath.Join(homeDir, LegacyDBFileName),
		SQLiteDB:     filepath.Join(homeDir, SQLiteFileName),
		BackupsDir:   backupsDir,
		MigrationLog: filepath.Join(homeDir, "migration.log"),
		AppCacheFile: filepath.Join(homeDir, "steam-app-cache.json"),
	}, nil
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// tightenPermissions makes a directory private to its owner if it is not
// already, on the platforms where mode bits are the access control.
//
// Best-effort: a failure here is not worth refusing to start over, and on
// Windows the call is a no-op in all but name — the profile folder's ACL is
// what protects the data there.
func tightenPermissions(dir string) {
	if runtime.GOOS == "windows" {
		return
	}
	info, err := os.Stat(dir)
	if err != nil {
		return
	}
	if info.Mode().Perm()&0o077 != 0 {
		_ = os.Chmod(dir, 0o700)
	}
}
