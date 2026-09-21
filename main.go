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
	"syscall"
	"time"
)

const version = "0.2.0"

func main() {
	var (
		address  = flag.String("addr", envOr("VEYRA_HUB_ADDRESS", ":8787"), "listen address")
		dataDir  = flag.String("data", envOr("VEYRA_HUB_DATA", "./data"), "persistent data directory")
		username = flag.String("username", envOr("VEYRA_HUB_USERNAME", "admin"), "media server username")
		token    = flag.String("token", os.Getenv("VEYRA_HUB_TOKEN"), "API token (or VEYRA_HUB_TOKEN)")
	)
	flag.Parse()

	if *token == "" {
		fmt.Fprintln(os.Stderr, "VEYRA_HUB_TOKEN is required")
		os.Exit(2)
	}

	store, err := NewStore(filepath.Join(*dataDir, "hub.json"))
	if err != nil {
		slog.Error("cannot open hub store", "error", err)
		os.Exit(1)
	}

	hub := NewHub(store, *username, *token, nil)
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
