package api

import (
	"net/http"

	"rss-lance/server/db"
)

// CategoriesHandler handles /api/categories
type CategoriesHandler struct{ store db.Store }

func NewCategoriesHandler(s db.Store) *CategoriesHandler { return &CategoriesHandler{store: s} }

func (h *CategoriesHandler) Handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	cats, err := h.store.GetCategories()
	if err != nil {
		jsonError(w, err, http.StatusInternalServerError)
		return
	}
	if cats == nil {
		cats = []db.Category{}
	}
	jsonOK(w, cats)
}
