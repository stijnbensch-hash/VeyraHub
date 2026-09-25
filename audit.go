package main

import (
	"net/http"
	"time"
)

// AuditEntry is one recorded admin action, kept for accountability: who did
// what, to which resource, and when. VeyraHub has exactly one admin
// account, but the actor is still recorded explicitly (id + username)
// rather than assumed, since a username can change and an entry should
// keep reading correctly regardless.
type AuditEntry struct {
	ID         string    `json:"id"`
	ActorID    string    `json:"actorID"`
	ActorName  string    `json:"actorName"`
	Action     string    `json:"action"`
	TargetType string    `json:"targetType,omitempty"`
	TargetID   string    `json:"targetID,omitempty"`
	Detail     string    `json:"detail,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
}

// maxAuditEntries caps how much audit history hub.json carries. Once
// exceeded, the oldest entries are dropped: recent accountability is what
// matters operationally, and an unbounded log would otherwise grow
// hub.json forever on a long-running hub.
const maxAuditEntries = 2000

func (s *Store) AppendAudit(entry AuditEntry) {
	entry.ID = randomID()
	entry.CreatedAt = time.Now().UTC()

	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.AuditLog = append(s.state.AuditLog, entry)
	if len(s.state.AuditLog) > maxAuditEntries {
		s.state.AuditLog = s.state.AuditLog[len(s.state.AuditLog)-maxAuditEntries:]
	}
	_ = s.persistLocked()
}

// AuditEntries returns the recorded audit log, most recent first.
func (s *Store) AuditEntries() []AuditEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]AuditEntry, len(s.state.AuditLog))
	for i, entry := range s.state.AuditLog {
		result[len(result)-1-i] = entry
	}
	return result
}

// audit records one admin action against the account making the current
// request. It's called from admin-only handlers (reached exclusively via
// requireAdmin), so the actor is always taken from the request context
// rather than passed in separately.
func (h *Hub) audit(r *http.Request, action, targetType, targetID, detail string) {
	admin, _ := r.Context().Value(ctxUserKey).(User)
	h.store.AppendAudit(AuditEntry{
		ActorID: admin.ID, ActorName: admin.Username,
		Action: action, TargetType: targetType, TargetID: targetID, Detail: detail,
	})
}

func (h *Hub) registerAuditRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/audit-log", h.requireAdmin(h.listAuditLog))
}

func (h *Hub) listAuditLog(w http.ResponseWriter, r *http.Request) {
	entries := h.store.AuditEntries()
	if entries == nil {
		entries = []AuditEntry{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
}
