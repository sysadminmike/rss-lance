package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"rss-lance/server/db"
)

// ArticlesHandler handles /api/articles/{id}/...
type ArticlesHandler struct {
	store  db.Store
	logger *ServerLogger
}

func NewArticlesHandler(s db.Store) *ArticlesHandler { return &ArticlesHandler{store: s} }

func (h *ArticlesHandler) SetLogger(l *ServerLogger) { h.logger = l }

func (h *ArticlesHandler) Handle(w http.ResponseWriter, r *http.Request) {
	// /api/articles/{id}[/read|/starred|/unread]
	path := strings.TrimPrefix(r.URL.Path, "/api/articles/")
	parts := strings.SplitN(path, "/", 2)
	articleID := parts[0]
	action := ""
	if len(parts) == 2 {
		action = parts[1]
	}

	if articleID == "" {
		// GET /api/articles - all articles, paginated
		if r.Method == http.MethodGet {
			h.listAll(w, r)
			return
		}
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if articleID == "batch" && r.Method == http.MethodPost {
		h.getBatch(w, r)
		return
	}

	switch {
	case action == "" && r.Method == http.MethodGet:
		h.getArticle(w, r, articleID)

	case action == "read" && r.Method == http.MethodPost:
		h.setRead(w, r, articleID, true)

	case action == "unread" && r.Method == http.MethodPost:
		h.setRead(w, r, articleID, false)

	case action == "star" && r.Method == http.MethodPost:
		h.setStarred(w, r, articleID, true)

	case action == "unstar" && r.Method == http.MethodPost:
		h.setStarred(w, r, articleID, false)

	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

func (h *ArticlesHandler) listAll(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit      := queryInt(q.Get("limit"),  50)
	offset     := queryInt(q.Get("offset"), 0)
	unreadOnly := q.Get("unread") == "true"
	sortAsc    := q.Get("sort") == "asc"

	arts, err := h.store.GetArticles("", limit, offset, unreadOnly, sortAsc)
	if err != nil {
		jsonError(w, err, http.StatusInternalServerError)
		return
	}
	if arts == nil {
		arts = []db.Article{}
	}
	jsonOK(w, arts)
}

func (h *ArticlesHandler) getArticle(w http.ResponseWriter, _ *http.Request, id string) {
	art, err := h.store.GetArticle(id)
	if err != nil {
		jsonError(w, err, http.StatusInternalServerError)
		return
	}
	if art == nil {
		http.Error(w, "article not found", http.StatusNotFound)
		return
	}
	jsonOK(w, art)
}

func (h *ArticlesHandler) setRead(w http.ResponseWriter, _ *http.Request, id string, read bool) {
	if err := h.store.SetArticleRead(id, read); err != nil {
		jsonError(w, err, http.StatusInternalServerError)
		return
	}
	if h.logger != nil {
		action := "read"
		if !read {
			action = "unread"
		}
		h.logger.LogJSON("info", "article_actions", "Article marked "+action,
			map[string]any{"article_id": id, "read": read})
	}
	jsonOK(w, map[string]bool{"ok": true})
}

func (h *ArticlesHandler) setStarred(w http.ResponseWriter, _ *http.Request, id string, starred bool) {
	if err := h.store.SetArticleStarred(id, starred); err != nil {
		jsonError(w, err, http.StatusInternalServerError)
		return
	}
	if h.logger != nil {
		action := "starred"
		if !starred {
			action = "unstarred"
		}
		h.logger.LogJSON("info", "article_actions", "Article "+action,
			map[string]any{"article_id": id, "starred": starred})
	}
	jsonOK(w, map[string]bool{"ok": true})
}

func (h *ArticlesHandler) getBatch(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IDs []string `json:"ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.IDs) == 0 {
		http.Error(w, `body must contain {"ids": [...]}`, http.StatusBadRequest)
		return
	}
	// Cap at 100 to prevent abuse
	if len(body.IDs) > 100 {
		body.IDs = body.IDs[:100]
	}
	arts, err := h.store.GetArticleBatch(body.IDs)
	if err != nil {
		jsonError(w, err, http.StatusInternalServerError)
		return
	}
	if arts == nil {
		arts = []db.Article{}
	}
	jsonOK(w, arts)
}
