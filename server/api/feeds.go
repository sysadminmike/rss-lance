// Package api implements the REST handlers for RSS-Lance.
package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"rss-lance/server/db"
)

// FeedsHandler handles /api/feeds and /api/feeds/{id}/...
type FeedsHandler struct {
	store  db.Store
	logger *ServerLogger
}

func NewFeedsHandler(s db.Store) *FeedsHandler { return &FeedsHandler{store: s} }

func (h *FeedsHandler) SetLogger(l *ServerLogger) { h.logger = l }

func (h *FeedsHandler) Handle(w http.ResponseWriter, r *http.Request) {
	// Strip leading /api/feeds
	path := strings.TrimPrefix(r.URL.Path, "/api/feeds")
	path = strings.Trim(path, "/")
	parts := strings.SplitN(path, "/", 2)

	switch {
	case path == "" && r.Method == http.MethodGet:
		h.listFeeds(w, r)

	case path == "" && r.Method == http.MethodPost:
		h.addFeed(w, r)

	case len(parts) == 1 && parts[0] != "" && r.Method == http.MethodGet:
		h.getFeed(w, r, parts[0])

	case len(parts) == 1 && parts[0] != "" && r.Method == http.MethodDelete:
		h.deleteFeed(w, r, parts[0])

	case len(parts) == 2 && parts[1] == "articles" && r.Method == http.MethodGet:
		h.listArticles(w, r, parts[0])

	case len(parts) == 2 && parts[1] == "mark-all-read" && r.Method == http.MethodPost:
		h.markAllRead(w, r, parts[0])

	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

func (h *FeedsHandler) listFeeds(w http.ResponseWriter, r *http.Request) {
	feeds, err := h.store.GetFeeds()
	if err != nil {
		jsonError(w, err, http.StatusInternalServerError)
		return
	}
	if feeds == nil {
		feeds = []db.Feed{}
	}
	jsonOK(w, feeds)
}

func (h *FeedsHandler) getFeed(w http.ResponseWriter, r *http.Request, id string) {
	feed, err := h.store.GetFeed(id)
	if err != nil {
		jsonError(w, err, http.StatusInternalServerError)
		return
	}
	if feed == nil {
		http.Error(w, "feed not found", http.StatusNotFound)
		return
	}
	jsonOK(w, feed)
}

func (h *FeedsHandler) addFeed(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URL        string `json:"url"`
		CategoryID string `json:"category_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.URL == "" {
		http.Error(w, "body must contain {\"url\": \"...\"}", http.StatusBadRequest)
		return
	}
	if err := h.store.QueueFeed(body.URL, body.CategoryID); err != nil {
		jsonError(w, err, http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusAccepted)
	jsonOK(w, map[string]string{
		"status":  "queued",
		"message": "Feed queued - the fetcher will add it on its next run.",
	})
	if h.logger != nil {
		h.logger.LogJSON("info", "feed_actions", "Feed queued: "+body.URL,
			map[string]any{"url": body.URL, "category_id": body.CategoryID})
	}
}

func (h *FeedsHandler) deleteFeed(w http.ResponseWriter, _ *http.Request, id string) {
	// Phase 1: queue a delete request (Python fetcher processes it)
	// For now return 501 until we implement the delete queue
	http.Error(w, "delete not yet implemented (Phase 2)", http.StatusNotImplemented)
}

func (h *FeedsHandler) listArticles(w http.ResponseWriter, r *http.Request, feedID string) {
	q := r.URL.Query()
	limit  := queryInt(q.Get("limit"),  50)
	offset := queryInt(q.Get("offset"), 0)
	unreadOnly := q.Get("unread") == "true"
	sortAsc    := q.Get("sort") == "asc"

	arts, err := h.store.GetArticles(feedID, limit, offset, unreadOnly, sortAsc)
	if err != nil {
		jsonError(w, err, http.StatusInternalServerError)
		return
	}
	if arts == nil {
		arts = []db.Article{}
	}
	jsonOK(w, arts)
}

func (h *FeedsHandler) markAllRead(w http.ResponseWriter, _ *http.Request, feedID string) {
	if err := h.store.MarkAllRead(feedID); err != nil {
		jsonError(w, err, http.StatusInternalServerError)
		return
	}
	if h.logger != nil {
		h.logger.LogJSON("info", "feed_actions", "Marked all read",
			map[string]any{"feed_id": feedID})
	}
	jsonOK(w, map[string]bool{"ok": true})
}
