package api

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"rss-lance/server/db"
)

// LogsHandler serves /api/logs endpoints.
type LogsHandler struct {
	store db.Store
}

func NewLogsHandler(store db.Store) *LogsHandler {
	return &LogsHandler{store: store}
}

func (h *LogsHandler) Handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	q := r.URL.Query()

	opts := db.LogQuery{
		Service:  q.Get("service"),
		Level:    q.Get("level"),
		Category: q.Get("category"),
		Limit:    queryInt(q.Get("limit"), 100),
		Offset:   queryInt(q.Get("offset"), 0),
	}
	if st := q.Get("start_time"); st != "" {
		if t, err := time.Parse(time.RFC3339, st); err == nil {
			opts.StartTime = &t
		}
	}
	if et := q.Get("end_time"); et != "" {
		if t, err := time.Parse(time.RFC3339, et); err == nil {
			opts.EndTime = &t
		}
	}

	// Validate service filter
	if opts.Service != "" && opts.Service != "api" && opts.Service != "fetcher" {
		jsonError(w, fmt.Errorf("invalid service: must be 'api', 'fetcher', or empty"), http.StatusBadRequest)
		return
	}

	// Validate level filter
	validLevels := map[string]bool{"": true, "debug": true, "info": true, "warn": true, "error": true}
	if !validLevels[opts.Level] {
		jsonError(w, fmt.Errorf("invalid level: must be debug, info, warn, error, or empty"), http.StatusBadRequest)
		return
	}

	entries, total, err := h.store.GetLogs(opts)
	if err != nil {
		jsonError(w, err, http.StatusInternalServerError)
		return
	}

	jsonOK(w, map[string]any{
		"entries": entries,
		"total":   total,
		"limit":   opts.Limit,
		"offset":  opts.Offset,
	})
}

// ── Server logging helper ─────────────────────────────────────────────────────

// ServerLogger provides convenience methods for the API server to write
// structured log entries. It checks settings before writing.
type ServerLogger struct {
	store    db.Store
	settings map[string]bool
}

func NewServerLogger(store db.Store) *ServerLogger {
	sl := &ServerLogger{store: store}
	sl.ReloadSettings()
	return sl
}

func (sl *ServerLogger) ReloadSettings() {
	settings, err := sl.store.GetSettings()
	if err != nil {
		return
	}
	sl.settings = make(map[string]bool)
	for k, v := range settings {
		if strings.HasPrefix(k, "log.api.") {
			sl.settings[k] = (v == "true" || v == `"true"` || v == "1")
		}
	}
}

func (sl *ServerLogger) shouldLog(category string) bool {
	if enabled, ok := sl.settings["log.api.enabled"]; ok && !enabled {
		return false
	}
	if cat, ok := sl.settings["log.api."+category]; ok {
		return cat
	}
	return false
}

func (sl *ServerLogger) Log(level, category, message, details string) {
	if !sl.shouldLog(category) {
		return
	}
	now := time.Now().UTC()
	entry := db.LogEntry{
		LogID:     newUUID(),
		Timestamp: &now,
		Level:     level,
		Category:  category,
		Message:   message,
		Details:   details,
	}
	// WriteLog buffers entries and flushes in batches - non-blocking
	sl.store.WriteLog(entry)
}

func (sl *ServerLogger) LogJSON(level, category, message string, data map[string]any) {
	if !sl.shouldLog(category) {
		return
	}
	details, _ := json.Marshal(data)
	sl.Log(level, category, message, string(details))
}

func newUUID() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
