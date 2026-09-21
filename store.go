package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Addon struct {
	ID          string         `json:"id"`
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Version     string         `json:"version,omitempty"`
	ManifestURL string         `json:"manifestURL"`
	BaseURL     string         `json:"baseURL"`
	Resources   []string       `json:"resources,omitempty"`
	Catalogs    []AddonCatalog `json:"catalogs,omitempty"`
	Enabled     bool           `json:"enabled"`
	AddedAt     time.Time      `json:"addedAt"`
}

type AddonCatalog struct {
	ID    string   `json:"id"`
	Type  string   `json:"type"`
	Name  string   `json:"name"`
	Extra []string `json:"extra,omitempty"`
}

type State struct {
	NodeID string  `json:"nodeID"`
	Addons []Addon `json:"addons"`
}

type Store struct {
	mu    sync.RWMutex
	path  string
	state State
}

func NewStore(path string) (*Store, error) {
	s := &Store{path: path}
	data, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(data, &s.state); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if s.state.NodeID == "" {
		s.state.NodeID = randomID()
		if err := s.persistLocked(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *Store) Snapshot() State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	copyState := s.state
	copyState.Addons = append([]Addon{}, s.state.Addons...)
	return copyState
}

func (s *Store) PutAddon(addon Addon) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.state.Addons {
		if s.state.Addons[i].ID == addon.ID {
			addon.AddedAt = s.state.Addons[i].AddedAt
			s.state.Addons[i] = addon
			return s.persistLocked()
		}
	}
	s.state.Addons = append(s.state.Addons, addon)
	return s.persistLocked()
}

func (s *Store) SetEnabled(id string, enabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.state.Addons {
		if s.state.Addons[i].ID == id {
			s.state.Addons[i].Enabled = enabled
			return s.persistLocked()
		}
	}
	return os.ErrNotExist
}

func (s *Store) DeleteAddon(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.state.Addons {
		if s.state.Addons[i].ID == id {
			s.state.Addons = append(s.state.Addons[:i], s.state.Addons[i+1:]...)
			return s.persistLocked()
		}
	}
	return os.ErrNotExist
}

func (s *Store) MoveAddon(id string, direction int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for index := range s.state.Addons {
		if s.state.Addons[index].ID != id {
			continue
		}
		target := index + direction
		if target < 0 || target >= len(s.state.Addons) {
			return nil
		}
		s.state.Addons[index], s.state.Addons[target] = s.state.Addons[target], s.state.Addons[index]
		return s.persistLocked()
	}
	return os.ErrNotExist
}

func (s *Store) persistLocked() error {
	data, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	temporary := s.path + ".tmp"
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, s.path)
}

func randomID() string {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		panic(err)
	}
	return hex.EncodeToString(value)
}
