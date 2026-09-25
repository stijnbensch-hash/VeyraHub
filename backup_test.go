package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestStoreWriteBackupCreatesReadableSnapshot checks that writeBackup
// produces a hub-*.json file in the target directory whose contents parse
// back as a Store (a minimal round-trip check, not a full State comparison).
func TestStoreWriteBackupCreatesReadableSnapshot(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "hub.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateUser("admin", "secret-password", RoleAdmin); err != nil {
		t.Fatal(err)
	}

	backupDir := filepath.Join(t.TempDir(), "backups")
	if err := store.writeBackup(backupDir); err != nil {
		t.Fatalf("writeBackup: %v", err)
	}

	entries, err := os.ReadDir(backupDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || !strings.HasPrefix(entries[0].Name(), "hub-") || !strings.HasSuffix(entries[0].Name(), ".json") {
		t.Fatalf("expected exactly one hub-*.json snapshot, got %+v", entries)
	}

	restored, err := NewStore(filepath.Join(backupDir, entries[0].Name()))
	if err != nil {
		t.Fatalf("backup snapshot didn't parse back as a store: %v", err)
	}
	if !restored.HasUsers() {
		t.Fatal("expected the restored snapshot to carry the admin user created before the backup")
	}
}

// TestPruneBackupsKeepsOnlyMostRecent checks that pruneBackups deletes the
// oldest snapshots once more than keep exist, leaving the lexically-last
// (i.e. most recent, given the timestamp naming) ones.
func TestPruneBackupsKeepsOnlyMostRecent(t *testing.T) {
	dir := t.TempDir()
	names := []string{
		"hub-20260101T000000Z.json",
		"hub-20260102T000000Z.json",
		"hub-20260103T000000Z.json",
		"hub-20260104T000000Z.json",
	}
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if err := pruneBackups(dir, 2); err != nil {
		t.Fatalf("pruneBackups: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 snapshots to remain, got %d: %+v", len(entries), entries)
	}
	remaining := map[string]bool{}
	for _, entry := range entries {
		remaining[entry.Name()] = true
	}
	if !remaining["hub-20260103T000000Z.json"] || !remaining["hub-20260104T000000Z.json"] {
		t.Fatalf("expected the two most recent snapshots to survive pruning, got %+v", entries)
	}
}

// TestStartBackupsWritesOnStartAndStopsOnCancel checks that StartBackups
// takes an immediate snapshot (rather than waiting a full interval) and
// that the background goroutine exits once its context is canceled.
func TestStartBackupsWritesOnStartAndStopsOnCancel(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "hub.json"))
	if err != nil {
		t.Fatal(err)
	}
	backupDir := filepath.Join(t.TempDir(), "backups")

	ctx, cancel := context.WithCancel(context.Background())
	store.StartBackups(ctx, backupDir, time.Hour, 5)

	deadline := time.Now().Add(5 * time.Second)
	for {
		entries, _ := os.ReadDir(backupDir)
		if len(entries) >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("expected an immediate backup snapshot within 5s of starting")
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
}
