package main

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// loginLimiter throttles repeated failed login attempts per (client IP,
// username) pair, on both login doors that check a password
// (/v1/auth/login and the Jellyfin-compatibility /Users/AuthenticateByName)
// — slowing down credential-stuffing/brute-force attempts without locking
// out a legitimate user just because someone else is hammering their
// username from a different network.
type loginLimiter struct {
	mu       sync.Mutex
	attempts map[string]*loginAttemptState
}

type loginAttemptState struct {
	failures      int
	lastFailureAt time.Time
	lockedUntil   time.Time
}

const (
	loginMaxFailures  = 5
	loginLockDuration = 5 * time.Minute

	// Failed attempts older than this no longer count toward the lockout
	// threshold, so a handful of typos spread across a day never lock
	// anyone out.
	loginFailureWindow = 15 * time.Minute
)

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{attempts: make(map[string]*loginAttemptState)}
}

func loginLimiterKey(ip, username string) string {
	return strings.ToLower(strings.TrimSpace(username)) + "\x00" + ip
}

// locked reports whether this (ip, username) pair is currently locked out,
// and if so for how much longer.
func (l *loginLimiter) locked(ip, username string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	state, ok := l.attempts[loginLimiterKey(ip, username)]
	if !ok {
		return false, 0
	}
	remaining := time.Until(state.lockedUntil)
	if remaining <= 0 {
		return false, 0
	}
	return true, remaining
}

// recordFailure counts one failed attempt, locking the (ip, username) pair
// out once loginMaxFailures is reached within loginFailureWindow.
func (l *loginLimiter) recordFailure(ip, username string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	key := loginLimiterKey(ip, username)
	now := time.Now()

	state, ok := l.attempts[key]
	if !ok || now.Sub(state.lastFailureAt) > loginFailureWindow {
		state = &loginAttemptState{}
		l.attempts[key] = state
	}

	state.failures++
	state.lastFailureAt = now

	if state.failures >= loginMaxFailures {
		state.lockedUntil = now.Add(loginLockDuration)
	}

	// Opportunistic cleanup so a long-running process doesn't accumulate
	// unbounded entries from a scanner hitting many usernames/IPs.
	if len(l.attempts) > 1024 {
		for k, v := range l.attempts {
			if now.Sub(v.lastFailureAt) > loginFailureWindow && now.After(v.lockedUntil) {
				delete(l.attempts, k)
			}
		}
	}
}

// recordSuccess clears any failure history for this (ip, username) pair.
func (l *loginLimiter) recordSuccess(ip, username string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.attempts, loginLimiterKey(ip, username))
}

// clientIP extracts the caller's address for rate-limiting purposes.
// VeyraHub is normally reached through a reverse proxy (e.g. Caddy, as in
// the deployment guide), which replaces the raw TCP peer with its own
// address unless it forwards the original client via X-Forwarded-For — so
// that header is preferred when present. This is a best-effort signal for
// slowing a brute-force scanner, not a hard trust boundary: a client that
// reaches the hub directly (bypassing any proxy) could spoof this header,
// same as most single-instance self-hosted setups without a fixed set of
// trusted proxy IPs to validate against.
func clientIP(r *http.Request) string {
	if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
		if ip := strings.TrimSpace(strings.Split(forwarded, ",")[0]); ip != "" {
			return ip
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
