// SPDX-FileCopyrightText: Copyright 2026 Eden Emulator Project
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

// envString reads the Game Save setting first. The former EDEN_SYNCD_* names
// remain accepted so an in-place container upgrade does not require rewriting
// secrets before the new image has proved healthy.
func envString(name, legacyName, fallback string) string {
	return envStringAny(fallback, name, legacyName)
}

func envStringAny(fallback string, names ...string) string {
	for _, name := range names {
		if name == "" {
			continue
		}
		if value, ok := os.LookupEnv(name); ok && value != "" {
			return value
		}
	}
	return fallback
}

func envInt(name, legacyName string, fallback int) int {
	for _, candidate := range []string{name, legacyName} {
		if candidate == "" {
			continue
		}
		if value, err := strconv.Atoi(os.Getenv(candidate)); err == nil {
			return value
		}
	}
	return fallback
}

func envDuration(name, legacyName string, fallback time.Duration) time.Duration {
	for _, candidate := range []string{name, legacyName} {
		if candidate == "" {
			continue
		}
		if value, err := time.ParseDuration(os.Getenv(candidate)); err == nil {
			return value
		}
	}
	return fallback
}

// runHealthcheck lets container runtimes probe the service without a shell or HTTP client in
// the image.
func runHealthcheck(arguments []string) int {
	flags := flag.NewFlagSet("healthcheck", flag.ExitOnError)
	target := flags.String("url", envString("GAME_SAVE_HEALTH_URL", "EDEN_SYNCD_HEALTH_URL", "http://127.0.0.1:8787/healthz"),
		"health endpoint to probe")
	_ = flags.Parse(arguments)
	client := http.Client{Timeout: 3 * time.Second}
	response, err := client.Get(*target)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "unhealthy:", response.Status)
		return 1
	}
	return 0
}

// runRebuildHeads re-creates a missing current version for every SaveVault
// scope of one vault, straight from the archives on disk. It needs no token and
// no HTTP client, so an operator can repair a copied or half-lost data
// directory from inside the container:
//
//	docker exec game-save game-save rebuild-heads --vault 123 --dry-run
func runRebuildHeads(arguments []string) int {
	flags := flag.NewFlagSet("rebuild-heads", flag.ExitOnError)
	root := flags.String("data-dir", envString("GAME_SAVE_DATA", "EDEN_SYNCD_DATA", "./data"), "data directory")
	vault := flags.String("vault", defaultVaultID, "vault id (a directory under <data-dir>/vaults)")
	dryRun := flags.Bool("dry-run", false, "only report which heads would be rebuilt")
	_ = flags.Parse(arguments)

	dataDir := filepath.Join(*root, "vaults", *vault)
	if _, err := os.Stat(dataDir); err != nil {
		fmt.Fprintf(os.Stderr, "vault %q not found under %s: %v\n", *vault, *root, err)
		return 1
	}
	if !*dryRun {
		if err := migrateLegacySaveVaultData(dataDir); err != nil {
			fmt.Fprintln(os.Stderr, "migrate legacy OpenSave data:", err)
			return 1
		}
	}
	s := &server{
		dataDir:         dataDir,
		maxObjectBytes:  1 << 40,
		now:             time.Now,
		saveVaultManage: defaultSaveVaultManageState(),
	}
	if err := s.loadSaveVaultState(); err != nil {
		fmt.Fprintln(os.Stderr, "load SaveVault state:", err)
		return 1
	}
	if err := s.loadSaveVaultManage(); err != nil {
		fmt.Fprintln(os.Stderr, "load SaveVault manage state:", err)
		return 1
	}
	report, err := s.rebuildSaveVaultHeadsFromArchives(*dryRun)
	if err != nil {
		fmt.Fprintln(os.Stderr, "rebuild:", err)
		return 1
	}
	output, _ := json.MarshalIndent(report, "", "  ")
	fmt.Println(string(output))
	if report.Rebuilt > 0 && !*dryRun {
		fmt.Printf("rebuilt %d head(s) in vault %s\n", report.Rebuilt, *vault)
	}
	return 0
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(runHealthcheck(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == "rebuild-heads" {
		os.Exit(runRebuildHeads(os.Args[2:]))
	}

	dataDir := flag.String("data-dir", envString("GAME_SAVE_DATA", "EDEN_SYNCD_DATA", "./data"), "directory for objects and metadata")
	listenAddress := flag.String("listen", envString("GAME_SAVE_LISTEN", "EDEN_SYNCD_LISTEN", ":8787"), "HTTP listen address")
	token := flag.String("token", envString("GAME_SAVE_TOKEN", "EDEN_SYNCD_TOKEN", ""), "bearer token")
	adminToken := flag.String("admin-token", envString("GAME_SAVE_ADMIN_TOKEN", "EDEN_SYNCD_ADMIN_TOKEN", ""), "administrator token for the web dashboard")
	saveVaultUsername := flag.String("savevault-username",
		envStringAny("", "GAME_SAVE_SAVEVAULT_USERNAME", "GAME_SAVE_OPENSAVE_USERNAME", "EDEN_SYNCD_SAVEVAULT_USERNAME", "EDEN_SYNCD_OPENSAVE_USERNAME"),
		"SaveVault WebDAV username")
	saveVaultPassword := flag.String("savevault-password",
		envStringAny("", "GAME_SAVE_SAVEVAULT_PASSWORD", "GAME_SAVE_OPENSAVE_PASSWORD", "EDEN_SYNCD_SAVEVAULT_PASSWORD", "EDEN_SYNCD_OPENSAVE_PASSWORD"),
		"SaveVault WebDAV password")
	legacyOpenSaveUsername := flag.String("opensave-username", "", "deprecated alias for --savevault-username")
	legacyOpenSavePassword := flag.String("opensave-password", "", "deprecated alias for --savevault-password")
	barkURL := flag.String("bark-url", envString("GAME_SAVE_BARK_URL", "EDEN_SYNCD_BARK_URL", ""),
		"Bark base URL including the device path")
	barkGroup := flag.String("bark-group", envString("GAME_SAVE_BARK_GROUP", "EDEN_SYNCD_BARK_GROUP", "game-save"),
		"Bark notification group")
	barkTTL := flag.Int("bark-ttl", envInt("GAME_SAVE_BARK_TTL", "EDEN_SYNCD_BARK_TTL", defaultBarkTTL),
		"Bark notification TTL in seconds")
	barkEvents := flag.String("bark-events", envString("GAME_SAVE_BARK_EVENTS", "EDEN_SYNCD_BARK_EVENTS", eventConflict),
		"comma-separated Bark event kinds")
	barkTimeout := flag.Duration("bark-timeout", envDuration("GAME_SAVE_BARK_TIMEOUT", "EDEN_SYNCD_BARK_TIMEOUT", defaultBarkTimeout),
		"Bark request timeout")
	maxObjectMiB := flag.Int64("max-object-mib", int64(envInt("GAME_SAVE_MAX_OBJECT_MIB", "EDEN_SYNCD_MAX_OBJECT_MIB",
		int(defaultMaxObjectBytes/(1024*1024)))), "maximum object size in MiB")
	keepSnapshots := flag.Int("keep-snapshots", envInt("GAME_SAVE_KEEP_SNAPSHOTS", "EDEN_SYNCD_KEEP_SNAPSHOTS", 0),
		"initial retention: newest snapshots kept per scope (0 = unlimited; dashboard settings take precedence)")
	keepDays := flag.Int("keep-days", envInt("GAME_SAVE_KEEP_DAYS", "EDEN_SYNCD_KEEP_DAYS", 0),
		"initial retention: keep snapshots younger than this many days (0 = unlimited)")
	maintenanceInterval := flag.Duration("maintenance-interval",
		envDuration("GAME_SAVE_MAINTENANCE_INTERVAL", "EDEN_SYNCD_MAINTENANCE_INTERVAL", 6*time.Hour),
		"how often retention and object garbage collection run (0 disables)")
	logFormat := flag.String("log-format", envString("GAME_SAVE_LOG_FORMAT", "EDEN_SYNCD_LOG_FORMAT", "text"), "log format: text or json")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}
	if *saveVaultUsername == "" && *legacyOpenSaveUsername != "" {
		*saveVaultUsername = *legacyOpenSaveUsername
	}
	if *saveVaultPassword == "" && *legacyOpenSavePassword != "" {
		*saveVaultPassword = *legacyOpenSavePassword
	}
	var logHandler slog.Handler = slog.NewTextHandler(os.Stderr, nil)
	if *logFormat == "json" {
		logHandler = slog.NewJSONHandler(os.Stderr, nil)
	}
	slog.SetDefault(slog.New(logHandler))

	vaults, err := newVaultRegistry(*dataDir, *token, *maxObjectMiB*1024*1024, serverOptions{
		adminToken:       *adminToken,
		saveVaultUsername: *saveVaultUsername,
		saveVaultPassword: *saveVaultPassword,
		settings: serverSettings{Retention: retentionPolicy{
			KeepSnapshots: *keepSnapshots,
			KeepDays:      *keepDays,
		}},
		bark: barkConfig{
			Endpoint: *barkURL,
			Group:    *barkGroup,
			TTL:      *barkTTL,
			Events:   *barkEvents,
			Timeout:  *barkTimeout,
		},
	})
	if err != nil {
		slog.Error("start", "error", err)
		os.Exit(1)
	}
	defer vaults.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Maintain every active vault (including tenants created after boot).
	if *maintenanceInterval > 0 {
		go vaults.maintenanceLoop(ctx, *maintenanceInterval)
	}

	httpServer := &http.Server{
		Addr:              *listenAddress,
		Handler:           vaults.handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	go func() {
		<-ctx.Done()
		shutdownContext, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownContext); err != nil {
			slog.Warn("shutdown", "error", err)
		}
	}()

	slog.Info("game-save listening", "address", *listenAddress, "version", version,
		"data_dir", *dataDir, "dashboard", *adminToken != "", "savevault_webdav", *saveVaultUsername != "")
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("serve", "error", err)
		os.Exit(1)
	}
	slog.Info("game-save stopped")
}
