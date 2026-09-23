package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	RoleAdmin  = "admin"
	RoleViewer = "viewer"

	accessTokenTTL  = 12 * time.Hour
	refreshTokenTTL = 30 * 24 * time.Hour

	// pbkdf2Iterations follows OWASP's current guidance for PBKDF2-HMAC-SHA256.
	pbkdf2Iterations = 210000
	pbkdf2KeyLength  = 32
)

var (
	ErrInvalidCredentials = errors.New("invalid credentials")
	ErrSessionInvalid     = errors.New("session invalid")
)

type Addon struct {
	ID           string         `json:"id"`
	Name         string         `json:"name"`
	Description  string         `json:"description,omitempty"`
	Version      string         `json:"version,omitempty"`
	ManifestURL  string         `json:"manifestURL"`
	BaseURL      string         `json:"baseURL"`
	ConfigureURL string         `json:"configureURL,omitempty"`
	Resources    []string       `json:"resources,omitempty"`
	Catalogs     []AddonCatalog `json:"catalogs,omitempty"`
	Enabled      bool           `json:"enabled"`
	AddedAt      time.Time      `json:"addedAt"`

	// Health reflects the addon's own reachability, kept separate from
	// Enabled: enabled/disabled is an admin choice, health is what the hub
	// has observed. Never populated from anything but the hub's own calls,
	// and LastError is a sanitized, addon-safe message — never a raw error
	// that might echo back a manifest URL or query-string credential.
	Reachable     bool       `json:"reachable"`
	LastSuccessAt *time.Time `json:"lastSuccessAt,omitempty"`
	LastErrorAt   *time.Time `json:"lastErrorAt,omitempty"`
	LastError     string     `json:"lastError,omitempty"`
}

type AddonCatalog struct {
	ID    string   `json:"id"`
	Type  string   `json:"type"`
	Name  string   `json:"name"`
	Extra []string `json:"extra,omitempty"`
}

// User is an authenticated account on the hub: either the private admin
// account or a personal viewing account used by a Veyra client.
type User struct {
	ID           string    `json:"id"`
	Username     string    `json:"username"`
	PasswordHash string    `json:"passwordHash"`
	Role         string    `json:"role"`
	Enabled      bool      `json:"enabled"`
	CreatedAt    time.Time `json:"createdAt"`
}

// Session is one logged-in device for a user. Only hashes of the access and
// refresh tokens are persisted; the plain tokens are returned once, at
// login/refresh time, and never stored.
type Session struct {
	ID               string     `json:"id"`
	UserID           string     `json:"userID"`
	DeviceID         string     `json:"deviceID"`
	DeviceName       string     `json:"deviceName,omitempty"`
	AccessTokenHash  string     `json:"accessTokenHash"`
	RefreshTokenHash string     `json:"refreshTokenHash"`
	CreatedAt        time.Time  `json:"createdAt"`
	AccessExpiresAt  time.Time  `json:"accessExpiresAt"`
	RefreshExpiresAt time.Time  `json:"refreshExpiresAt"`
	RevokedAt        *time.Time `json:"revokedAt,omitempty"`
}

func (s Session) active(now time.Time) bool {
	return s.RevokedAt == nil && now.Before(s.RefreshExpiresAt)
}

type UserAddonSet struct {
	UserID string  `json:"userID"`
	Addons []Addon `json:"addons"`
}

type State struct {
	NodeID     string          `json:"nodeID"`
	Addons     []Addon         `json:"addons,omitempty"`
	UserAddons []UserAddonSet  `json:"userAddons,omitempty"`
	Users      []User          `json:"users,omitempty"`
	Sessions   []Session       `json:"sessions,omitempty"`
	VeyraSync  []VeyraUserSync `json:"veyraSync,omitempty"`
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
	dirty := false
	if s.state.NodeID == "" {
		s.state.NodeID = randomID()
		dirty = true
	}
	if s.migrateLegacyAddons() {
		dirty = true
	}
	if dirty {
		if err := s.persistLocked(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// migrateLegacyAddons moves addons from the old global "addons" list (used
// before addons moved into the per-account data model) into the admin
// account's addon set. This is a one-time upgrade path for a hub.json
// written by an older build; it's a no-op once the global list is empty,
// which is every run after the first on an upgraded hub. Called from
// NewStore before any concurrent access starts, so it touches state
// directly without locking.
func (s *Store) migrateLegacyAddons() bool {
	if len(s.state.Addons) == 0 {
		return false
	}
	var adminID string
	for _, user := range s.state.Users {
		if user.Role == RoleAdmin {
			adminID = user.ID
			break
		}
	}
	if adminID == "" {
		// No admin account yet (shouldn't normally happen alongside a
		// populated legacy addon list); leave it in place and retry on
		// the next start rather than silently discarding it.
		return false
	}
	set := s.ensureUserAddonsLocked(adminID)
	set.Addons = append(set.Addons, s.state.Addons...)
	s.state.Addons = nil
	return true
}

// AdminUserID returns the id of the hub's single admin account. Addons are
// stored per-account, but only the admin manages them, so every consumer —
// admin or viewer, Jellyfin bridge or native API — resolves the shared
// addon catalog against this id rather than the requesting account's own.
func (s *Store) AdminUserID() (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, user := range s.state.Users {
		if user.Role == RoleAdmin {
			return user.ID, true
		}
	}
	return "", false
}

func (s *Store) Snapshot() State {
	s.mu.RLock()
	defer s.mu.RUnlock()

	copyState := s.state
	copyState.Addons = append([]Addon{}, s.state.Addons...)
	copyState.UserAddons = cloneUserAddonSets(s.state.UserAddons)
	copyState.Users = append([]User{}, s.state.Users...)
	copyState.Sessions = append([]Session{}, s.state.Sessions...)
	copyState.VeyraSync = append([]VeyraUserSync{}, s.state.VeyraSync...)

	return copyState
}

func cloneUserAddonSets(values []UserAddonSet) []UserAddonSet {
	result := make([]UserAddonSet, len(values))

	for i, value := range values {
		result[i] = value
		result[i].Addons = append([]Addon{}, value.Addons...)
	}

	return result
}

// HasUsers reports whether any account has been created yet, used to decide
// whether to bootstrap the initial admin account on startup.
func (s *Store) HasUsers() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.state.Users) > 0
}

// CreateUser creates a new account with a hashed password. Returns
// os.ErrExist if the username is already taken.
func (s *Store) CreateUser(username, password, role string) (User, error) {
	hash, err := hashPassword(password)
	if err != nil {
		return User{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, user := range s.state.Users {
		if strings.EqualFold(user.Username, username) {
			return User{}, os.ErrExist
		}
	}
	user := User{
		ID: randomID(), Username: username, PasswordHash: hash,
		Role: role, Enabled: true, CreatedAt: time.Now().UTC(),
	}
	s.state.Users = append(s.state.Users, user)
	if err := s.persistLocked(); err != nil {
		return User{}, err
	}
	return user, nil
}

func (s *Store) SetUserEnabled(id string, enabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.state.Users {
		if s.state.Users[i].ID == id {
			s.state.Users[i].Enabled = enabled
			return s.persistLocked()
		}
	}
	return os.ErrNotExist
}

// SetUserPassword replaces a user's password (used by admin-initiated
// resets). Existing sessions are left as-is; call RevokeUserSessions
// separately if they should be signed out.
func (s *Store) SetUserPassword(id, password string) error {
	hash, err := hashPassword(password)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.state.Users {
		if s.state.Users[i].ID == id {
			s.state.Users[i].PasswordHash = hash
			return s.persistLocked()
		}
	}
	return os.ErrNotExist
}

func (s *Store) DeleteUser(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	found := false
	for i := range s.state.Users {
		if s.state.Users[i].ID == id {
			s.state.Users = append(s.state.Users[:i], s.state.Users[i+1:]...)
			found = true
			break
		}
	}
	if !found {
		return os.ErrNotExist
	}
	kept := s.state.Sessions[:0]
	for _, session := range s.state.Sessions {
		if session.UserID != id {
			kept = append(kept, session)
		}
	}
	s.state.Sessions = kept

	syncKept := s.state.VeyraSync[:0]
	for _, syncState := range s.state.VeyraSync {
		if syncState.UserID != id {
			syncKept = append(syncKept, syncState)
		}
	}
	s.state.VeyraSync = syncKept

	return s.persistLocked()
}

// UserByID looks up a user by id (locked; safe to call from HTTP handlers).
func (s *Store) UserByID(id string) (User, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.userByID(id)
}

func (s *Store) userByID(id string) (User, bool) {
	for _, user := range s.state.Users {
		if user.ID == id {
			return user, true
		}
	}
	return User{}, false
}

// Authenticate checks a username/password pair against stored accounts.
func (s *Store) Authenticate(username, password string) (User, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, user := range s.state.Users {
		if !strings.EqualFold(user.Username, username) {
			continue
		}
		if !user.Enabled {
			return User{}, false
		}
		if !verifyPassword(user.PasswordHash, password) {
			return User{}, false
		}
		return user, true
	}
	return User{}, false
}

// CreateSession issues a fresh access/refresh token pair for a device and
// persists only their hashes.
func (s *Store) CreateSession(userID, deviceID, deviceName string) (Session, string, string, error) {
	if deviceID == "" {
		deviceID = randomID()
	}
	accessToken := randomToken()
	refreshToken := randomToken()
	now := time.Now().UTC()
	session := Session{
		ID: randomID(), UserID: userID, DeviceID: deviceID, DeviceName: deviceName,
		AccessTokenHash: tokenHash(accessToken), RefreshTokenHash: tokenHash(refreshToken),
		CreatedAt: now, AccessExpiresAt: now.Add(accessTokenTTL), RefreshExpiresAt: now.Add(refreshTokenTTL),
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Sessions = append(s.state.Sessions, session)
	if err := s.persistLocked(); err != nil {
		return Session{}, "", "", err
	}
	return session, accessToken, refreshToken, nil
}

// SessionByAccessToken resolves a live session and its user from a bearer
// access token. Expired or revoked sessions, and sessions for a disabled or
// deleted user, are rejected.
func (s *Store) SessionByAccessToken(token string) (Session, User, bool) {
	if token == "" {
		return Session{}, User{}, false
	}
	wantedHash := tokenHash(token)
	now := time.Now().UTC()
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, session := range s.state.Sessions {
		if !secureHashEqual(session.AccessTokenHash, wantedHash) {
			continue
		}
		if session.RevokedAt != nil || now.After(session.AccessExpiresAt) {
			return Session{}, User{}, false
		}
		user, ok := s.userByID(session.UserID)
		if !ok || !user.Enabled {
			return Session{}, User{}, false
		}
		return session, user, true
	}
	return Session{}, User{}, false
}

// RefreshSession rotates a session's tokens given a valid, unexpired refresh
// token, extending the device's login without asking for the password again.
func (s *Store) RefreshSession(refreshToken string) (Session, User, string, string, error) {
	wantedHash := tokenHash(refreshToken)
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.state.Sessions {
		session := &s.state.Sessions[i]
		if !secureHashEqual(session.RefreshTokenHash, wantedHash) {
			continue
		}
		if !session.active(now) {
			return Session{}, User{}, "", "", ErrSessionInvalid
		}
		user, ok := s.userByID(session.UserID)
		if !ok || !user.Enabled {
			return Session{}, User{}, "", "", ErrSessionInvalid
		}
		accessToken := randomToken()
		refreshTokenNew := randomToken()
		session.AccessTokenHash = tokenHash(accessToken)
		session.RefreshTokenHash = tokenHash(refreshTokenNew)
		session.AccessExpiresAt = now.Add(accessTokenTTL)
		session.RefreshExpiresAt = now.Add(refreshTokenTTL)
		if err := s.persistLocked(); err != nil {
			return Session{}, User{}, "", "", err
		}
		return *session, user, accessToken, refreshTokenNew, nil
	}
	return Session{}, User{}, "", "", ErrSessionInvalid
}

// RevokeSession signs a single device out. Only the session owner or an
// admin should be allowed to call this (enforced by the HTTP layer).
func (s *Store) RevokeSession(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.state.Sessions {
		if s.state.Sessions[i].ID == id {
			if s.state.Sessions[i].RevokedAt != nil {
				return nil
			}
			now := time.Now().UTC()
			s.state.Sessions[i].RevokedAt = &now
			return s.persistLocked()
		}
	}
	return os.ErrNotExist
}

// RevokeUserSessions signs every device for a user out at once, e.g. after a
// password reset.
func (s *Store) RevokeUserSessions(userID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	changed := false
	for i := range s.state.Sessions {
		if s.state.Sessions[i].UserID == userID && s.state.Sessions[i].RevokedAt == nil {
			s.state.Sessions[i].RevokedAt = &now
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return s.persistLocked()
}

// SessionsForUser lists the live (non-expired, non-revoked) sessions for a
// user, i.e. their signed-in devices.
func (s *Store) SessionsForUser(userID string) []Session {
	now := time.Now().UTC()
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []Session
	for _, session := range s.state.Sessions {
		if session.UserID == userID && session.active(now) {
			result = append(result, session)
		}
	}
	return result
}

// AllSessions lists every live session across all users, for the admin
// dashboard.
func (s *Store) AllSessions() []Session {
	now := time.Now().UTC()
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []Session
	for _, session := range s.state.Sessions {
		if session.active(now) {
			result = append(result, session)
		}
	}
	return result
}

func (s *Store) userAddonIndexLocked(userID string) int {
	for i := range s.state.UserAddons {
		if s.state.UserAddons[i].UserID == userID {
			return i
		}
	}
	return -1
}

func (s *Store) ensureUserAddonsLocked(userID string) *UserAddonSet {
	index := s.userAddonIndexLocked(userID)
	if index >= 0 {
		return &s.state.UserAddons[index]
	}

	s.state.UserAddons = append(s.state.UserAddons, UserAddonSet{
		UserID: userID,
		Addons: []Addon{},
	})

	return &s.state.UserAddons[len(s.state.UserAddons)-1]
}

func (s *Store) AddonsForUser(userID string) []Addon {
	s.mu.RLock()
	defer s.mu.RUnlock()

	index := s.userAddonIndexLocked(userID)
	if index < 0 {
		return []Addon{}
	}

	return append([]Addon{}, s.state.UserAddons[index].Addons...)
}

func (s *Store) PutAddonForUser(userID string, addon Addon) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	set := s.ensureUserAddonsLocked(userID)

	for i := range set.Addons {
		if set.Addons[i].ID == addon.ID {
			addon.AddedAt = set.Addons[i].AddedAt
			set.Addons[i] = addon
			return s.persistLocked()
		}
	}

	set.Addons = append(set.Addons, addon)
	return s.persistLocked()
}

func (s *Store) SetAddonEnabledForUser(userID, id string, enabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	index := s.userAddonIndexLocked(userID)
	if index < 0 {
		return os.ErrNotExist
	}

	for i := range s.state.UserAddons[index].Addons {
		if s.state.UserAddons[index].Addons[i].ID == id {
			s.state.UserAddons[index].Addons[i].Enabled = enabled
			return s.persistLocked()
		}
	}

	return os.ErrNotExist
}

func (s *Store) RecordAddonHealthForUser(userID, id string, ok bool, message string) {
	now := time.Now().UTC()

	s.mu.Lock()
	defer s.mu.Unlock()

	index := s.userAddonIndexLocked(userID)
	if index < 0 {
		return
	}

	for i := range s.state.UserAddons[index].Addons {
		addon := &s.state.UserAddons[index].Addons[i]
		if addon.ID != id {
			continue
		}

		addon.Reachable = ok

		if ok {
			addon.LastSuccessAt = &now
		} else {
			addon.LastErrorAt = &now
			addon.LastError = message
		}

		_ = s.persistLocked()
		return
	}
}

func (s *Store) DeleteAddonForUser(userID, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	index := s.userAddonIndexLocked(userID)
	if index < 0 {
		return os.ErrNotExist
	}

	addons := s.state.UserAddons[index].Addons

	for i := range addons {
		if addons[i].ID == id {
			s.state.UserAddons[index].Addons =
				append(addons[:i], addons[i+1:]...)

			return s.persistLocked()
		}
	}

	return os.ErrNotExist
}

func (s *Store) MoveAddonForUser(userID, id string, direction int) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	index := s.userAddonIndexLocked(userID)
	if index < 0 {
		return os.ErrNotExist
	}

	addons := s.state.UserAddons[index].Addons

	for current := range addons {
		if addons[current].ID != id {
			continue
		}

		target := current + direction

		if target < 0 || target >= len(addons) {
			return nil
		}

		addons[current], addons[target] =
			addons[target], addons[current]

		s.state.UserAddons[index].Addons = addons

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

func randomToken() string {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		panic(err)
	}
	return hex.EncodeToString(value)
}

func tokenHash(token string) string {
	value := sha256.Sum256([]byte(token))
	return hex.EncodeToString(value[:])
}

func secureHashEqual(a, b string) bool {
	return len(a) == len(b) && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// hashPassword derives a salted PBKDF2-HMAC-SHA256 hash for storage, encoded
// as "pbkdf2-sha256$<iterations>$<saltHex>$<hashHex>". Implemented against
// the standard library only (no golang.org/x/crypto) since this environment
// cannot reach the Go module proxy.
func hashPassword(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	derived := pbkdf2HMACSHA256([]byte(password), salt, pbkdf2Iterations, pbkdf2KeyLength)
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", pbkdf2Iterations, hex.EncodeToString(salt), hex.EncodeToString(derived)), nil
}

// verifyPassword checks a plaintext password against an encoded hash
// produced by hashPassword, in constant time.
func verifyPassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	iterations, err := strconv.Atoi(parts[1])
	if err != nil || iterations <= 0 {
		return false
	}
	salt, err := hex.DecodeString(parts[2])
	if err != nil {
		return false
	}
	wanted, err := hex.DecodeString(parts[3])
	if err != nil {
		return false
	}
	got := pbkdf2HMACSHA256([]byte(password), salt, iterations, len(wanted))
	return subtle.ConstantTimeCompare(got, wanted) == 1
}

// pbkdf2HMACSHA256 implements PBKDF2 (RFC 8018) with HMAC-SHA256 as the PRF.
func pbkdf2HMACSHA256(password, salt []byte, iterations, keyLength int) []byte {
	prf := hmac.New(sha256.New, password)
	hashLength := prf.Size()
	numBlocks := (keyLength + hashLength - 1) / hashLength
	derived := make([]byte, 0, numBlocks*hashLength)
	blockIndex := make([]byte, 4)
	for block := 1; block <= numBlocks; block++ {
		prf.Reset()
		prf.Write(salt)
		binary.BigEndian.PutUint32(blockIndex, uint32(block))
		prf.Write(blockIndex)
		u := prf.Sum(nil)
		t := make([]byte, len(u))
		copy(t, u)
		for i := 1; i < iterations; i++ {
			prf.Reset()
			prf.Write(u)
			u = prf.Sum(nil)
			for j := range t {
				t[j] ^= u[j]
			}
		}
		derived = append(derived, t...)
	}
	return derived[:keyLength]
}
