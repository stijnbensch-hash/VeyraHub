package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// backupTimestampLayout produces names that sort chronologically as plain
// strings (no separators that would confuse filesystem globbing) and are
// still readable at a glance.
const backupTimestampLayout = "20060102T150405Z"

// StartBackups periodically writes a timestamped snapshot of the store's
// current contents into dir, pruning older snapshots beyond keep. It runs
// in its own goroutine until ctx is canceled. This is entirely opt-in,
// started once from main after the store is opened — a bare NewStore
// (as used throughout the test suite) never starts backups on its own.
func (s *Store) StartBackups(ctx context.Context, dir string, interval time.Duration, keep int) {
	if interval <= 0 || keep <= 0 {
		return
	}
	go func() {
		// Take one backup right away rather than waiting a full interval
		// after a fresh restart, so a hub that's restarted often still
		// gets regular snapshots.
		s.backupOnce(dir, keep)

		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.backupOnce(dir, keep)
			}
		}
	}()
}

func (s *Store) backupOnce(dir string, keep int) {
	if err := s.writeBackup(dir); err != nil {
		slog.Warn("hub.json backup failed", "error", err)
		return
	}
	if err := pruneBackups(dir, keep); err != nil {
		slog.Warn("hub.json backup cleanup failed", "error", err)
	}
}

// writeBackup writes a timestamped copy of the store's current contents
// into dir. It re-marshals the in-memory state under the store's own lock
// rather than copying the on-disk hub.json file, so the snapshot is always
// a single consistent point in time even if a write happens concurrently.
func (s *Store) writeBackup(dir string) error {
	s.mu.RLock()
	data, err := json.MarshalIndent(s.state, "", "  ")
	s.mu.RUnlock()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(dir, fmt.Sprintf("hub-%s.json", time.Now().UTC().Format(backupTimestampLayout)))
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

// pruneBackups keeps only the keep most recent hub-*.json snapshots in dir,
// deleting older ones. Names are timestamp-formatted so lexical sort order
// is also chronological order.
func pruneBackups(dir string, keep int) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var names []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.Type().IsRegular() && strings.HasPrefix(name, "hub-") && strings.HasSuffix(name, ".json") {
			names = append(names, name)
		}
	}
	if len(names) <= keep {
		return nil
	}
	sort.Strings(names)
	for _, name := range names[:len(names)-keep] {
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			return err
		}
	}
	return nil
}
