package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"rss-lance/server/db"
)

// SettingsHandler handles /api/settings and /api/settings/{key}
type SettingsHandler struct {
	store  db.Store
	logger *ServerLogger
}

func NewSettingsHandler(s db.Store) *SettingsHandler { return &SettingsHandler{store: s} }

func (h *SettingsHandler) SetLogger(l *ServerLogger) { h.logger = l }

func (h *SettingsHandler) Handle(w http.ResponseWriter, r *http.Request) {
	// Strip leading /api/settings
	path := strings.TrimPrefix(r.URL.Path, "/api/settings")
	key := strings.Trim(path, "/")

	switch {
	case key == "" && r.Method == http.MethodGet:
		h.getAll(w, r)

	case key == "" && r.Method == http.MethodPut:
		h.putBatch(w, r)

	case key != "" && r.Method == http.MethodGet:
		h.getOne(w, r, key)

	case key != "" && r.Method == http.MethodPut:
		h.putOne(w, r, key)

	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

// GET /api/settings — all settings as {key: json_value, ...}
func (h *SettingsHandler) getAll(w http.ResponseWriter, _ *http.Request) {
	settings, err := h.store.GetSettings()
	if err != nil {
		jsonError(w, err, http.StatusInternalServerError)
		return
	}
	if settings == nil {
		settings = map[string]string{}
	}
	// Values are JSON-encoded strings; decode them for the response
	result := make(map[string]any, len(settings))
	for k, v := range settings {
		var parsed any
		if err := json.Unmarshal([]byte(v), &parsed); err != nil {
			parsed = v // fallback: return raw string
		}
		result[k] = parsed
	}
	jsonOK(w, result)
}

// GET /api/settings/{key} — single setting
func (h *SettingsHandler) getOne(w http.ResponseWriter, _ *http.Request, key string) {
	val, found, err := h.store.GetSetting(key)
	if err != nil {
		jsonError(w, err, http.StatusInternalServerError)
		return
	}
	if !found {
		http.Error(w, "setting not found", http.StatusNotFound)
		return
	}
	var parsed any
	if err := json.Unmarshal([]byte(val), &parsed); err != nil {
		parsed = val
	}
	jsonOK(w, map[string]any{"key": key, "value": parsed})
}

// PUT /api/settings — batch update: {"key1": value1, "key2": value2}
func (h *SettingsHandler) putBatch(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	// Encode all values to JSON strings for storage
	encoded := make(map[string]string, len(body))
	for k, v := range body {
		b, err := json.Marshal(v)
		if err != nil {
			http.Error(w, "cannot encode value for key: "+k, http.StatusBadRequest)
			return
		}
		encoded[k] = string(b)
	}

	// Single batch write — groups by value internally, typically 2-3 UPDATEs total
	if err := h.store.PutSettings(encoded); err != nil {
		jsonError(w, err, http.StatusInternalServerError)
		return
	}

	// Post-save: log changes and reload log settings if any log.* keys changed
	if h.logger != nil {
		hasLogKey := false
		for k, v := range body {
			h.logger.LogJSON("info", "settings_changes", "Setting changed: "+k,
				map[string]any{"key": k, "value": v})
			if strings.HasPrefix(k, "log.") {
				hasLogKey = true
			}
		}
		if hasLogKey {
			h.logger.ReloadSettings()
		}
	}
	jsonOK(w, map[string]bool{"ok": true})
}

// PUT /api/settings/{key} — single update: {"value": ...}
func (h *SettingsHandler) putOne(w http.ResponseWriter, r *http.Request, key string) {
	var body struct {
		Value any `json:"value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	encoded, err := json.Marshal(body.Value)
	if err != nil {
		http.Error(w, "cannot encode value", http.StatusBadRequest)
		return
	}
	if err := h.store.PutSetting(key, string(encoded)); err != nil {
		jsonError(w, err, http.StatusInternalServerError)
		return
	}
	if h.logger != nil {
		h.logger.LogJSON("info", "settings_changes", "Setting changed: "+key,
			map[string]any{"key": key, "value": body.Value})
		if strings.HasPrefix(key, "log.") {
			h.logger.ReloadSettings()
		}
	}
	jsonOK(w, map[string]bool{"ok": true})
}
