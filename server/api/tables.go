package api

import (
	"net/http"
	"strings"

	"rss-lance/server/db"
)

// TablesHandler handles GET /api/tables/{name}
type TablesHandler struct{ store db.Store }

func NewTablesHandler(s db.Store) *TablesHandler { return &TablesHandler{store: s} }

func (h *TablesHandler) Handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	table := strings.TrimPrefix(r.URL.Path, "/api/tables/")
	if table == "" {
		// Return the list of available tables
		jsonOK(w, map[string]any{
			"tables": []string{"articles", "feeds", "categories", "pending_feeds", "settings", "log_api", "log_fetcher"},
		})
		return
	}

	q := r.URL.Query()
	limit := queryInt(q.Get("limit"), 200)
	offset := queryInt(q.Get("offset"), 0)
	if limit > 5000 {
		limit = 5000
	}

	result, err := h.store.QueryTable(table, limit, offset)
	if err != nil {
		jsonError(w, err, http.StatusBadRequest)
		return
	}
	jsonOK(w, result)
}
