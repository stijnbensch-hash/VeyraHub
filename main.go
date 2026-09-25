package main

import (
	"context"
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

const version = "0.2.0"

func main() {
	var (
		address  = flag.String("addr", envOr("VEYRA_HUB_ADDRESS", ":8787"), "listen address")
		dataDir  = flag.String("data", envOr("VEYRA_HUB_DATA", "./data"), "persistent data directory")
		username = flag.String("username", envOr("VEYRA_HUB_USERNAME", "admin"), "initial admin username (only used to bootstrap the first account)")
		token    = flag.String("token", os.Getenv("VEYRA_HUB_TOKEN"), "initial admin password (or VEYRA_HUB_TOKEN); only used to bootstrap the first account")

		// Push notifications via APNs are optional: when apnsKeyPath is
		// empty (the default), the hub runs exactly as before and
		// notifications only ever reach the poll-based feed.
		apnsKeyPath    = flag.String("apns-key-path", envOr("VEYRA_HUB_APNS_KEY_PATH", ""), "path to an APNs authentication key (.p8); leave empty to disable push")
		apnsKeyID      = flag.String("apns-key-id", envOr("VEYRA_HUB_APNS_KEY_ID", ""), "APNs authentication key ID")
		apnsTeamID     = flag.String("apns-team-id", envOr("VEYRA_HUB_APNS_TEAM_ID", ""), "Apple Developer team ID")
		apnsBundleID   = flag.String("apns-bundle-id", envOr("VEYRA_HUB_APNS_BUNDLE_ID", ""), "Veyra app bundle ID (apns-topic)")
		apnsProduction = flag.Bool("apns-production", envOr("VEYRA_HUB_APNS_ENVIRONMENT", "sandbox") == "production", "use APNs' production environment instead of sandbox")

		// Automatic hub.json backups are on by default (a fresh snapshot
		// every 6h, most recent 28 kept — about a week of history at that
		// cadence); set backupInterval to 0 to disable entirely.
		backupInterval = flag.Duration("backup-interval", envDuration("VEYRA_HUB_BACKUP_INTERVAL", 6*time.Hour), "how often to write a hub.json backup snapshot (0 disables)")
		backupKeep     = flag.Int("backup-keep", envInt("VEYRA_HUB_BACKUP_KEEP", 28), "how many backup snapshots to retain")
	)
	flag.Parse()

	if *token == "" {
		fmt.Fprintln(os.Stderr, "VEYRA_HUB_TOKEN is required")
		os.Exit(2)
	}
	if len(*token) < minPasswordLength {
		fmt.Fprintf(os.Stderr, "VEYRA_HUB_TOKEN must be at least %d characters\n", minPasswordLength)
		os.Exit(2)
	}

	store, err := NewStore(filepath.Join(*dataDir, "hub.json"))
	if err != nil {
		slog.Error("cannot open hub store", "error", err)
		os.Exit(1)
	}

	apnsConfig, err := loadAPNsConfig(*apnsKeyPath, *apnsKeyID, *apnsTeamID, *apnsBundleID, *apnsProduction)
	if err != nil {
		fmt.Fprintln(os.Stderr, "apns configuratie ongeldig:", err)
		os.Exit(2)
	}
	if apnsConfig.configured() {
		slog.Info("apns push enabled", "production", apnsConfig.Production)
	} else {
		slog.Info("apns push disabled (no apns-key-path configured)")
	}

	hub := NewHub(store, *username, *token, nil)
	hub.EnableAPNs(apnsConfig)

	backupCtx, cancelBackups := context.WithCancel(context.Background())
	defer cancelBackups()
	if *backupInterval > 0 {
		backupDir := filepath.Join(*dataDir, "backups")
		store.StartBackups(backupCtx, backupDir, *backupInterval, *backupKeep)
		slog.Info("hub.json backups enabled", "interval", backupInterval.String(), "keep", *backupKeep, "dir", backupDir)
	} else {
		slog.Info("hub.json backups disabled")
	}

	server := &http.Server{
		Addr:              *address,
		Handler:           hub.Routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       20 * time.Second,
		WriteTimeout:      40 * time.Second,
		IdleTimeout:       90 * time.Second,
	}

	done := make(chan os.Signal, 1)
	signal.Notify(done, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-done
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			slog.Error("shutdown failed", "error", err)
		}
	}()

	slog.Info("Veyra Hub listening", "address", *address, "version", version)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) time.Duration {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func envInt(key string, fallback int) int {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}
