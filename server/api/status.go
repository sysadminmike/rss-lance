package api

import (
	"net/http"

	"rss-lance/server/db"
)

// StatusHandler handles GET /api/status
type StatusHandler struct{ store db.Store }

func NewStatusHandler(s db.Store) *StatusHandler { return &StatusHandler{store: s} }

func (h *StatusHandler) Handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	status, err := h.store.GetDBStatus()
	if err != nil {
		jsonError(w, err, http.StatusInternalServerError)
		return
	}
	jsonOK(w, status)
}
