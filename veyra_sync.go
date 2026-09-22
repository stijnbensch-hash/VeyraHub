package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

var ErrSyncConflict = errors.New("sync version conflict")

type VeyraSyncDocument struct {
	Version   int64           `json:"version"`
	UpdatedAt time.Time       `json:"updatedAt,omitempty"`
	Data      json.RawMessage `json:"data"`
}

type VeyraDeviceSync struct {
	DeviceID   string            `json:"deviceID"`
	DeviceName string            `json:"deviceName,omitempty"`
	Platform   string            `json:"platform,omitempty"`
	Settings   VeyraSyncDocument `json:"settings"`
}

type VeyraUserSync struct {
	UserID   string            `json:"userID"`
	Settings VeyraSyncDocument `json:"settings"`
	LiveTV   VeyraSyncDocument `json:"liveTV"`
	Devices  []VeyraDeviceSync `json:"devices,omitempty"`
}

type veyraSyncPutRequest struct {
	BaseVersion int64           `json:"baseVersion"`
	Platform    string          `json:"platform,omitempty"`
	Data        json.RawMessage `json:"data"`
}

func emptySyncDocument() VeyraSyncDocument {
	return VeyraSyncDocument{
		Version: 0,
		Data:    json.RawMessage(`{}`),
	}
}

func cloneSyncDocument(value VeyraSyncDocument) VeyraSyncDocument {
	result := value

	if len(value.Data) == 0 {
		result.Data = json.RawMessage(`{}`)
	} else {
		result.Data = append(json.RawMessage(nil), value.Data...)
	}

	return result
}

func cloneVeyraUserSync(value VeyraUserSync) VeyraUserSync {
	result := value

	result.Settings = cloneSyncDocument(value.Settings)
	result.LiveTV = cloneSyncDocument(value.LiveTV)

	result.Devices = make([]VeyraDeviceSync, len(value.Devices))

	for i, device := range value.Devices {
		result.Devices[i] = device
		result.Devices[i].Settings = cloneSyncDocument(device.Settings)
	}

	return result
}

func normalizeSyncData(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, errors.New("data ontbreekt")
	}

	var object map[string]json.RawMessage

	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, errors.New("data moet een geldig JSON-object zijn")
	}

	if object == nil {
		return nil, errors.New("data moet een JSON-object zijn")
	}

	normalized, err := json.Marshal(object)
	if err != nil {
		return nil, err
	}

	return normalized, nil
}

func (s *Store) veyraSyncIndexLocked(userID string) int {
	for i := range s.state.VeyraSync {
		if s.state.VeyraSync[i].UserID == userID {
			return i
		}
	}
	return -1
}

func (s *Store) ensureVeyraSyncLocked(userID string) *VeyraUserSync {
	index := s.veyraSyncIndexLocked(userID)

	if index >= 0 {
		return &s.state.VeyraSync[index]
	}

	s.state.VeyraSync = append(
		s.state.VeyraSync,
		VeyraUserSync{
			UserID:   userID,
			Settings: emptySyncDocument(),
			LiveTV:   emptySyncDocument(),
		},
	)

	return &s.state.VeyraSync[len(s.state.VeyraSync)-1]
}

func (s *Store) VeyraSyncForUser(userID string) VeyraUserSync {
	s.mu.RLock()
	defer s.mu.RUnlock()

	index := s.veyraSyncIndexLocked(userID)

	if index < 0 {
		return VeyraUserSync{
			UserID:   userID,
			Settings: emptySyncDocument(),
			LiveTV:   emptySyncDocument(),
			Devices:  []VeyraDeviceSync{},
		}
	}

	return cloneVeyraUserSync(
		s.state.VeyraSync[index],
	)
}

func updateSyncDocument(
	current *VeyraSyncDocument,
	baseVersion int64,
	data json.RawMessage,
) (VeyraSyncDocument, error) {
	if len(current.Data) == 0 {
		current.Data = json.RawMessage(`{}`)
	}

	if baseVersion != current.Version {
		return cloneSyncDocument(*current), ErrSyncConflict
	}

	current.Version++
	current.UpdatedAt = time.Now().UTC()
	current.Data = append(
		json.RawMessage(nil),
		data...,
	)

	return cloneSyncDocument(*current), nil
}

func (s *Store) PutVeyraSettings(
	userID string,
	baseVersion int64,
	data json.RawMessage,
) (VeyraSyncDocument, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	record := s.ensureVeyraSyncLocked(userID)

	result, err := updateSyncDocument(
		&record.Settings,
		baseVersion,
		data,
	)

	if err != nil {
		return result, err
	}

	if err := s.persistLocked(); err != nil {
		return VeyraSyncDocument{}, err
	}

	return result, nil
}

func (s *Store) PutVeyraLiveTV(
	userID string,
	baseVersion int64,
	data json.RawMessage,
) (VeyraSyncDocument, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	record := s.ensureVeyraSyncLocked(userID)

	result, err := updateSyncDocument(
		&record.LiveTV,
		baseVersion,
		data,
	)

	if err != nil {
		return result, err
	}

	if err := s.persistLocked(); err != nil {
		return VeyraSyncDocument{}, err
	}

	return result, nil
}

func (s *Store) VeyraDeviceForUser(
	userID string,
	deviceID string,
) VeyraDeviceSync {
	s.mu.RLock()
	defer s.mu.RUnlock()

	index := s.veyraSyncIndexLocked(userID)

	if index >= 0 {
		for _, device := range s.state.VeyraSync[index].Devices {
			if device.DeviceID == deviceID {
				result := device
				result.Settings = cloneSyncDocument(
					device.Settings,
				)
				return result
			}
		}
	}

	return VeyraDeviceSync{
		DeviceID: deviceID,
		Settings: emptySyncDocument(),
	}
}

func (s *Store) PutVeyraDevice(
	userID string,
	deviceID string,
	deviceName string,
	platform string,
	baseVersion int64,
	data json.RawMessage,
) (VeyraDeviceSync, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	record := s.ensureVeyraSyncLocked(userID)

	var device *VeyraDeviceSync

	for i := range record.Devices {
		if record.Devices[i].DeviceID == deviceID {
			device = &record.Devices[i]
			break
		}
	}

	if device == nil {
		record.Devices = append(
			record.Devices,
			VeyraDeviceSync{
				DeviceID:   deviceID,
				DeviceName: deviceName,
				Platform:   platform,
				Settings:   emptySyncDocument(),
			},
		)

		device = &record.Devices[len(record.Devices)-1]
	}

	result, err := updateSyncDocument(
		&device.Settings,
		baseVersion,
		data,
	)

	if err != nil {
		copy := *device
		copy.Settings = result
		return copy, err
	}

	if strings.TrimSpace(deviceName) != "" {
		device.DeviceName = strings.TrimSpace(deviceName)
	}

	if strings.TrimSpace(platform) != "" {
		device.Platform = strings.TrimSpace(platform)
	}

	if err := s.persistLocked(); err != nil {
		return VeyraDeviceSync{}, err
	}

	resultDevice := *device
	resultDevice.Settings = cloneSyncDocument(
		device.Settings,
	)

	return resultDevice, nil
}

func (h *Hub) registerVeyraSyncRoutes(mux *http.ServeMux) {
	mux.HandleFunc(
		"GET /veyra/v1/profile",
		h.requireSession(h.veyraSyncProfile),
	)

	mux.HandleFunc(
		"GET /veyra/v1/settings",
		h.requireSession(h.veyraGetSettings),
	)

	mux.HandleFunc(
		"PUT /veyra/v1/settings",
		h.requireSession(h.veyraPutSettings),
	)

	mux.HandleFunc(
		"GET /veyra/v1/livetv",
		h.requireSession(h.veyraGetLiveTV),
	)

	mux.HandleFunc(
		"PUT /veyra/v1/livetv",
		h.requireSession(h.veyraPutLiveTV),
	)

	mux.HandleFunc(
		"GET /veyra/v1/device",
		h.requireSession(h.veyraGetDevice),
	)

	mux.HandleFunc(
		"PUT /veyra/v1/device",
		h.requireSession(h.veyraPutDevice),
	)

	mux.HandleFunc(
		"GET /veyra/v1/devices",
		h.requireSession(h.veyraListDevices),
	)
}

func veyraRequestIdentity(
	r *http.Request,
) (User, Session) {
	user, _ := r.Context().
		Value(ctxUserKey).(User)

	session, _ := r.Context().
		Value(ctxSessionKey).(Session)

	return user, session
}

func (h *Hub) veyraSyncProfile(
	w http.ResponseWriter,
	r *http.Request,
) {
	user, _ := veyraRequestIdentity(r)

	writeJSON(
		w,
		http.StatusOK,
		h.store.VeyraSyncForUser(user.ID),
	)
}

func (h *Hub) veyraGetSettings(
	w http.ResponseWriter,
	r *http.Request,
) {
	user, _ := veyraRequestIdentity(r)

	profile :=
		h.store.VeyraSyncForUser(
			user.ID,
		)

	writeJSON(
		w,
		http.StatusOK,
		profile.Settings,
	)
}

func (h *Hub) veyraGetLiveTV(
	w http.ResponseWriter,
	r *http.Request,
) {
	user, _ := veyraRequestIdentity(r)

	profile :=
		h.store.VeyraSyncForUser(
			user.ID,
		)

	writeJSON(
		w,
		http.StatusOK,
		profile.LiveTV,
	)
}

func (h *Hub) veyraGetDevice(
	w http.ResponseWriter,
	r *http.Request,
) {
	user, session :=
		veyraRequestIdentity(r)

	device :=
		h.store.VeyraDeviceForUser(
			user.ID,
			session.DeviceID,
		)

	if device.DeviceName == "" {
		device.DeviceName =
			session.DeviceName
	}

	writeJSON(
		w,
		http.StatusOK,
		device,
	)
}

func (h *Hub) veyraListDevices(
	w http.ResponseWriter,
	r *http.Request,
) {
	user, _ := veyraRequestIdentity(r)

	profile :=
		h.store.VeyraSyncForUser(
			user.ID,
		)

	if profile.Devices == nil {
		profile.Devices =
			[]VeyraDeviceSync{}
	}

	writeJSON(
		w,
		http.StatusOK,
		map[string]any{
			"devices": profile.Devices,
			"count":   len(profile.Devices),
		},
	)
}

func (h *Hub) veyraPutSettings(
	w http.ResponseWriter,
	r *http.Request,
) {
	user, _ := veyraRequestIdentity(r)

	var input veyraSyncPutRequest

	if decodeJSON(r, &input) != nil {
		writeError(
			w,
			http.StatusBadRequest,
			"Ongeldige sync-aanvraag.",
		)
		return
	}

	data, err := normalizeSyncData(input.Data)

	if err != nil {
		writeError(
			w,
			http.StatusBadRequest,
			err.Error(),
		)
		return
	}

	document, err := h.store.PutVeyraSettings(
		user.ID,
		input.BaseVersion,
		data,
	)

	if errors.Is(err, ErrSyncConflict) {
		writeJSON(
			w,
			http.StatusConflict,
			map[string]any{
				"error":   "De instellingen zijn intussen op een ander apparaat gewijzigd.",
				"current": document,
			},
		)
		return
	}

	if err != nil {
		writeError(
			w,
			http.StatusInternalServerError,
			"Instellingen synchroniseren is mislukt.",
		)
		return
	}

	writeJSON(
		w,
		http.StatusOK,
		document,
	)
}

func (h *Hub) veyraPutLiveTV(
	w http.ResponseWriter,
	r *http.Request,
) {
	user, _ := veyraRequestIdentity(r)

	var input veyraSyncPutRequest

	if decodeJSON(r, &input) != nil {
		writeError(
			w,
			http.StatusBadRequest,
			"Ongeldige Live TV-syncaanvraag.",
		)
		return
	}

	data, err := normalizeSyncData(input.Data)

	if err != nil {
		writeError(
			w,
			http.StatusBadRequest,
			err.Error(),
		)
		return
	}

	document, err := h.store.PutVeyraLiveTV(
		user.ID,
		input.BaseVersion,
		data,
	)

	if errors.Is(err, ErrSyncConflict) {
		writeJSON(
			w,
			http.StatusConflict,
			map[string]any{
				"error":   "Live TV is intussen op een ander apparaat gewijzigd.",
				"current": document,
			},
		)
		return
	}

	if err != nil {
		writeError(
			w,
			http.StatusInternalServerError,
			"Live TV synchroniseren is mislukt.",
		)
		return
	}

	writeJSON(
		w,
		http.StatusOK,
		document,
	)
}

func (h *Hub) veyraPutDevice(
	w http.ResponseWriter,
	r *http.Request,
) {
	user, session := veyraRequestIdentity(r)

	var input veyraSyncPutRequest

	if decodeJSON(r, &input) != nil {
		writeError(
			w,
			http.StatusBadRequest,
			"Ongeldige device-syncaanvraag.",
		)
		return
	}

	data, err := normalizeSyncData(input.Data)

	if err != nil {
		writeError(
			w,
			http.StatusBadRequest,
			err.Error(),
		)
		return
	}

	device, err := h.store.PutVeyraDevice(
		user.ID,
		session.DeviceID,
		session.DeviceName,
		input.Platform,
		input.BaseVersion,
		data,
	)

	if errors.Is(err, ErrSyncConflict) {
		writeJSON(
			w,
			http.StatusConflict,
			map[string]any{
				"error":   "De apparaatinstellingen zijn intussen gewijzigd.",
				"current": device,
			},
		)
		return
	}

	if err != nil {
		writeError(
			w,
			http.StatusInternalServerError,
			"Apparaatinstellingen synchroniseren is mislukt.",
		)
		return
	}

	writeJSON(
		w,
		http.StatusOK,
		device,
	)
}
