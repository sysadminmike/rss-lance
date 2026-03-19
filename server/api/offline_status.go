package api

import (
	"net/http"

	"rss-lance/server/db"
)

// OfflineStatusHandler handles GET /api/offline-status
type OfflineStatusHandler struct{ store db.Store }

func NewOfflineStatusHandler(s db.Store) *OfflineStatusHandler {
	return &OfflineStatusHandler{store: s}
}

func (h *OfflineStatusHandler) Handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	st := h.store.OfflineStatus()
	if st == nil {
		// offCache not initialized (should not happen in normal operation)
		jsonOK(w, map[string]any{
			"offline": false,
		})
		return
	}
	jsonOK(w, map[string]any{
		"offline":         st.Offline,
		"pending_changes": st.PendingChanges,
		"pending_logs":    st.PendingLogs,
		"last_snapshot":   st.LastSnapshot,
		"cache_articles":  st.CacheArticles,
	})
}
