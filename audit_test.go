package main

import (
	"net/http"
	"testing"
)

// TestAuditLogRecordsAdminActions covers the audit-log feature: admin
// actions across users, addons and requests are recorded with the acting
// admin, in most-recent-first order, and reachable only to the admin.
func TestAuditLogRecordsAdminActions(t *testing.T) {
	server, _, adminToken := setupHubWithAddon(t)

	var user struct {
		ID string `json:"id"`
	}
	doJSON(t, http.MethodPost, server.URL+"/v1/users", adminToken,
		map[string]string{"username": "kid", "password": "kid-password"}, http.StatusCreated, &user)

	doJSON(t, http.MethodPatch, server.URL+"/v1/users/"+user.ID, adminToken,
		map[string]any{"enabled": false}, http.StatusNoContent, nil)

	doJSON(t, http.MethodDelete, server.URL+"/v1/users/"+user.ID, adminToken, nil, http.StatusNoContent, nil)

	var log struct {
		Entries []AuditEntry `json:"entries"`
	}
	doJSON(t, http.MethodGet, server.URL+"/v1/audit-log", adminToken, nil, http.StatusOK, &log)

	// setupHubWithAddon itself performs an addon.add, so at least that plus
	// the three user actions above should be present.
	if len(log.Entries) < 4 {
		t.Fatalf("expected at least 4 audit entries, got %d: %+v", len(log.Entries), log.Entries)
	}

	// Most recent first: the very last action taken was the user delete.
	if log.Entries[0].Action != "user.delete" {
		t.Fatalf("expected the newest entry to be user.delete, got %q", log.Entries[0].Action)
	}
	if log.Entries[0].TargetID != user.ID {
		t.Fatalf("expected the delete entry's targetID to be the deleted user, got %q", log.Entries[0].TargetID)
	}
	if log.Entries[0].ActorName != "admin" {
		t.Fatalf("expected the actor to be the admin account, got %q", log.Entries[0].ActorName)
	}

	var actions []string
	for _, entry := range log.Entries {
		actions = append(actions, entry.Action)
	}
	wantAny := map[string]bool{"user.create": false, "user.disable": false, "user.delete": false, "addon.add": false}
	for _, action := range actions {
		if _, ok := wantAny[action]; ok {
			wantAny[action] = true
		}
	}
	for action, seen := range wantAny {
		if !seen {
			t.Errorf("expected to find an audit entry for %q, actions were %v", action, actions)
		}
	}
}

// TestAuditLogRequiresAdmin ensures a viewer account (or an unauthenticated
// caller) can't read the audit log.
func TestAuditLogRequiresAdmin(t *testing.T) {
	server, _, adminToken := setupHubWithAddon(t)

	doJSON(t, http.MethodPost, server.URL+"/v1/users", adminToken,
		map[string]string{"username": "viewer", "password": "viewer-password"}, http.StatusCreated, nil)
	var login struct {
		AccessToken string `json:"accessToken"`
	}
	doJSON(t, http.MethodPost, server.URL+"/v1/auth/login", "",
		map[string]string{"username": "viewer", "password": "viewer-password"}, http.StatusOK, &login)

	doJSON(t, http.MethodGet, server.URL+"/v1/audit-log", login.AccessToken, nil, http.StatusForbidden, nil)
	doJSON(t, http.MethodGet, server.URL+"/v1/audit-log", "", nil, http.StatusUnauthorized, nil)
}
