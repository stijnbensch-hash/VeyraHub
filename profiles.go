package main

import (
	"net/http"
	"os"
	"strings"
	"time"
)

// Profile is a viewing profile under a viewer account (e.g. a parent's
// account with separate "Kids" and "Mij" profiles). Multiple Veyra clients
// signing in with the same account credentials can each pick a different
// profile; the hub tracks daily watch minutes per profile, not per account,
// so a kids' profile's screen-time limit doesn't affect the parent's own
// viewing on the same login.
type Profile struct {
	ID                string    `json:"id"`
	UserID            string    `json:"userID"`
	Name              string    `json:"name"`
	IsKid             bool      `json:"isKid"`
	DailyLimitMinutes int       `json:"dailyLimitMinutes,omitempty"` // 0 = no limit
	CreatedAt         time.Time `json:"createdAt"`
}

// ProfileUsage accumulates watched minutes for a profile on a single day
// (UTC, "2006-01-02"). The Veyra client reports elapsed playback time
// periodically via POST .../usage; the hub never measures playback itself.
type ProfileUsage struct {
	ProfileID string `json:"profileID"`
	Date      string `json:"date"`
	Minutes   int    `json:"minutes"`
}

func usageDateKey(t time.Time) string {
	return t.UTC().Format("2006-01-02")
}

func (s *Store) ProfilesForUser(userID string) []Profile {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []Profile
	for _, profile := range s.state.Profiles {
		if profile.UserID == userID {
			result = append(result, profile)
		}
	}
	return result
}

func (s *Store) ProfileByID(id string) (Profile, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, profile := range s.state.Profiles {
		if profile.ID == id {
			return profile, true
		}
	}
	return Profile{}, false
}

func (s *Store) CreateProfile(profile Profile) (Profile, error) {
	profile.ID = randomID()
	profile.CreatedAt = time.Now().UTC()

	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Profiles = append(s.state.Profiles, profile)
	if err := s.persistLocked(); err != nil {
		return Profile{}, err
	}
	return profile, nil
}

// DeleteProfile removes a profile owned by userID, along with its usage
// history. Returns os.ErrNotExist if no such profile belongs to that user
// (never lets one viewer delete another's profile by guessing an id).
func (s *Store) DeleteProfile(userID, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	found := false
	for i := range s.state.Profiles {
		if s.state.Profiles[i].ID == id && s.state.Profiles[i].UserID == userID {
			s.state.Profiles = append(s.state.Profiles[:i], s.state.Profiles[i+1:]...)
			found = true
			break
		}
	}
	if !found {
		return os.ErrNotExist
	}

	usageKept := s.state.ProfileUsage[:0]
	for _, usage := range s.state.ProfileUsage {
		if usage.ProfileID != id {
			usageKept = append(usageKept, usage)
		}
	}
	s.state.ProfileUsage = usageKept

	return s.persistLocked()
}

// AddProfileUsage records additional watched minutes for a profile today
// and returns the new running total for the day.
func (s *Store) AddProfileUsage(profileID string, minutes int) int {
	today := usageDateKey(time.Now())

	s.mu.Lock()
	defer s.mu.Unlock()

	for i := range s.state.ProfileUsage {
		if s.state.ProfileUsage[i].ProfileID == profileID && s.state.ProfileUsage[i].Date == today {
			s.state.ProfileUsage[i].Minutes += minutes
			total := s.state.ProfileUsage[i].Minutes
			_ = s.persistLocked()
			return total
		}
	}

	s.state.ProfileUsage = append(s.state.ProfileUsage, ProfileUsage{ProfileID: profileID, Date: today, Minutes: minutes})
	_ = s.persistLocked()
	return minutes
}

func (s *Store) UsageToday(profileID string) int {
	today := usageDateKey(time.Now())

	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, usage := range s.state.ProfileUsage {
		if usage.ProfileID == profileID && usage.Date == today {
			return usage.Minutes
		}
	}
	return 0
}

func (h *Hub) registerProfileRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/me/profiles", h.requireSession(h.listMyProfiles))
	mux.HandleFunc("POST /v1/me/profiles", h.requireSession(h.createMyProfile))
	mux.HandleFunc("DELETE /v1/me/profiles/{id}", h.requireSession(h.deleteMyProfile))
	mux.HandleFunc("GET /v1/me/profiles/{id}/usage", h.requireSession(h.getProfileUsage))
	mux.HandleFunc("POST /v1/me/profiles/{id}/usage", h.requireSession(h.addProfileUsage))
}

func (h *Hub) listMyProfiles(w http.ResponseWriter, r *http.Request) {
	user, _ := userFromContext(r)
	profiles := h.store.ProfilesForUser(user.ID)
	if profiles == nil {
		profiles = []Profile{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"profiles": profiles})
}

func (h *Hub) createMyProfile(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name              string `json:"name"`
		IsKid             bool   `json:"isKid"`
		DailyLimitMinutes int    `json:"dailyLimitMinutes"`
	}
	if decodeJSON(r, &input) != nil {
		writeError(w, http.StatusBadRequest, "Ongeldige aanvraag.")
		return
	}
	name := strings.TrimSpace(input.Name)
	if name == "" {
		writeError(w, http.StatusBadRequest, "Geef het profiel een naam.")
		return
	}
	if input.DailyLimitMinutes < 0 {
		writeError(w, http.StatusBadRequest, "De daglimiet kan niet negatief zijn.")
		return
	}
	user, _ := userFromContext(r)
	profile, err := h.store.CreateProfile(Profile{
		UserID: user.ID, Name: name, IsKid: input.IsKid, DailyLimitMinutes: input.DailyLimitMinutes,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Het profiel kon niet worden opgeslagen.")
		return
	}
	writeJSON(w, http.StatusCreated, profile)
}

func (h *Hub) deleteMyProfile(w http.ResponseWriter, r *http.Request) {
	user, _ := userFromContext(r)
	if err := h.store.DeleteProfile(user.ID, r.PathValue("id")); err != nil {
		writeError(w, http.StatusNotFound, "Profiel niet gevonden.")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// profileUsageResponse is shared by the GET and POST usage endpoints so a
// client always sees the same shape whether it's reporting or just polling.
func profileUsageResponse(profile Profile, usedMinutes int) map[string]any {
	response := map[string]any{
		"profileID":   profile.ID,
		"usedMinutes": usedMinutes,
	}
	if profile.DailyLimitMinutes > 0 {
		remaining := profile.DailyLimitMinutes - usedMinutes
		if remaining < 0 {
			remaining = 0
		}
		response["limitMinutes"] = profile.DailyLimitMinutes
		response["remainingMinutes"] = remaining
		response["limitReached"] = usedMinutes >= profile.DailyLimitMinutes
	} else {
		response["limitReached"] = false
	}
	return response
}

func (h *Hub) profileForOwnedRequest(w http.ResponseWriter, r *http.Request) (Profile, bool) {
	user, _ := userFromContext(r)
	profile, ok := h.store.ProfileByID(r.PathValue("id"))
	if !ok || profile.UserID != user.ID {
		writeError(w, http.StatusNotFound, "Profiel niet gevonden.")
		return Profile{}, false
	}
	return profile, true
}

func (h *Hub) getProfileUsage(w http.ResponseWriter, r *http.Request) {
	profile, ok := h.profileForOwnedRequest(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, profileUsageResponse(profile, h.store.UsageToday(profile.ID)))
}

func (h *Hub) addProfileUsage(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Minutes int `json:"minutes"`
	}
	if decodeJSON(r, &input) != nil || input.Minutes < 0 {
		writeError(w, http.StatusBadRequest, "Geef een niet-negatief aantal minuten op.")
		return
	}
	profile, ok := h.profileForOwnedRequest(w, r)
	if !ok {
		return
	}
	used := h.store.AddProfileUsage(profile.ID, input.Minutes)
	writeJSON(w, http.StatusOK, profileUsageResponse(profile, used))
}

// profileLimitExceeded checks the optional X-Veyra-Profile-Id header a
// client may send on playback requests (streams/PlaybackInfo) against that
// profile's own daily limit. Absent header or unlimited profile: never
// blocks — this is opt-in enforcement, not a requirement for clients that
// don't yet send profile context.
func (h *Hub) profileLimitExceeded(r *http.Request) bool {
	profileID := strings.TrimSpace(r.Header.Get("X-Veyra-Profile-Id"))
	if profileID == "" {
		return false
	}
	profile, ok := h.store.ProfileByID(profileID)
	if !ok || profile.DailyLimitMinutes <= 0 {
		return false
	}
	return h.store.UsageToday(profile.ID) >= profile.DailyLimitMinutes
}
