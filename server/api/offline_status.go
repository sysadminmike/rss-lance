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
		// Offline mode not enabled
		jsonOK(w, map[string]any{
			"enabled": false,
			"offline": false,
		})
		return
	}
	jsonOK(w, map[string]any{
		"enabled":         true,
		"offline":         st.Offline,
		"pending_changes": st.PendingChanges,
		"pending_logs":    st.PendingLogs,
		"last_snapshot":   st.LastSnapshot,
		"cache_articles":  st.CacheArticles,
	})
}
