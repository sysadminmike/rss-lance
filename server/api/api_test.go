package api

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"rss-lance/server/db"
)

// ── Mock Store ────────────────────────────────────────────────────────────────
// Implements db.Store entirely in memory. No CGo or native libs needed.

type mockStore struct {
	feeds      []db.Feed
	articles   []db.Article
	categories []db.Category
	pending    []string
	settings   map[string]string

	// Track calls for assertions
	readCalls    map[string]int
	writeCalls   map[string]int
	lastReadID   string
	lastStarred  *bool
	lastFeedURL  string
	queueErr     error
	markAllErr   error
	getArticleID string
}

func newMockStore() *mockStore {
	return &mockStore{
		readCalls:  make(map[string]int),
		writeCalls: make(map[string]int),
		settings:   make(map[string]string),
	}
}

func (m *mockStore) Close() error { return nil }

func (m *mockStore) GetFeeds() ([]db.Feed, error) {
	m.readCalls["GetFeeds"]++
	return m.feeds, nil
}

func (m *mockStore) GetFeed(id string) (*db.Feed, error) {
	m.readCalls["GetFeed"]++
	for i := range m.feeds {
		if m.feeds[i].FeedID == id {
			return &m.feeds[i], nil
		}
	}
	return nil, nil
}

func (m *mockStore) QueueFeed(url, categoryID string) error {
	m.writeCalls["QueueFeed"]++
	m.lastFeedURL = url
	return m.queueErr
}

func (m *mockStore) GetPendingFeeds() ([]string, error) {
	return m.pending, nil
}

func (m *mockStore) GetArticles(feedID string, limit, offset int, unreadOnly bool, sortAsc bool) ([]db.Article, error) {
	m.readCalls["GetArticles"]++
	result := make([]db.Article, len(m.articles))
	copy(result, m.articles)
	if feedID != "" {
		filtered := []db.Article{}
		for _, a := range result {
			if a.FeedID == feedID {
				filtered = append(filtered, a)
			}
		}
		result = filtered
	}
	if unreadOnly {
		filtered := []db.Article{}
		for _, a := range result {
			if !a.IsRead {
				filtered = append(filtered, a)
			}
		}
		result = filtered
	}
	if offset > len(result) {
		return []db.Article{}, nil
	}
	result = result[offset:]
	if limit < len(result) {
		result = result[:limit]
	}
	return result, nil
}

func (m *mockStore) GetArticle(id string) (*db.Article, error) {
	m.readCalls["GetArticle"]++
	m.getArticleID = id
	for i := range m.articles {
		if m.articles[i].ArticleID == id {
			return &m.articles[i], nil
		}
	}
	return nil, nil
}

func (m *mockStore) GetArticleBatch(ids []string) ([]db.Article, error) {
	m.readCalls["GetArticleBatch"]++
	idSet := make(map[string]bool, len(ids))
	for _, id := range ids {
		idSet[id] = true
	}
	var result []db.Article
	for _, a := range m.articles {
		if idSet[a.ArticleID] {
			result = append(result, a)
		}
	}
	return result, nil
}

func (m *mockStore) SetArticleRead(id string, isRead bool) error {
	m.writeCalls["SetArticleRead"]++
	m.lastReadID = id
	for i := range m.articles {
		if m.articles[i].ArticleID == id {
			m.articles[i].IsRead = isRead
			break
		}
	}
	return nil
}

func (m *mockStore) SetArticleStarred(id string, isStarred bool) error {
	m.writeCalls["SetArticleStarred"]++
	m.lastStarred = &isStarred
	for i := range m.articles {
		if m.articles[i].ArticleID == id {
			m.articles[i].IsStarred = isStarred
			break
		}
	}
	return nil
}

func (m *mockStore) MarkAllRead(feedID string) error {
	m.writeCalls["MarkAllRead"]++
	if m.markAllErr != nil {
		return m.markAllErr
	}
	for i := range m.articles {
		if m.articles[i].FeedID == feedID {
			m.articles[i].IsRead = true
		}
	}
	return nil
}

func (m *mockStore) GetCategories() ([]db.Category, error) {
	m.readCalls["GetCategories"]++
	return m.categories, nil
}

func (m *mockStore) GetSettings() (map[string]string, error) {
	m.readCalls["GetSettings"]++
	return m.settings, nil
}

func (m *mockStore) GetSetting(key string) (string, bool, error) {
	m.readCalls["GetSetting"]++
	v, ok := m.settings[key]
	return v, ok, nil
}

func (m *mockStore) PutSetting(key, value string) error {
	m.writeCalls["PutSetting"]++
	m.settings[key] = value
	return nil
}

func (m *mockStore) PutSettings(settings map[string]string) error {
	m.writeCalls["PutSettings"]++
	for k, v := range settings {
		m.settings[k] = v
	}
	return nil
}

func (m *mockStore) GetDBStatus() (*db.DBStatus, error) {
	m.readCalls["GetDBStatus"]++
	// Compute stats from in-memory articles
	total := len(m.articles)
	unread := 0
	starred := 0
	for _, a := range m.articles {
		if !a.IsRead {
			unread++
		}
		if a.IsStarred {
			starred++
		}
	}
	return &db.DBStatus{
		DataPath: "/tmp/test",
		Tables: []db.TableStats{
			{Name: "articles", RowCount: total, SizeBytes: 1024},
			{Name: "feeds", RowCount: len(m.feeds), SizeBytes: 512},
			{Name: "categories", RowCount: len(m.categories), SizeBytes: 256},
		},
		Articles: db.ArticleStats{
			Total:   total,
			Unread:  unread,
			Starred: starred,
			Oldest:  "2024-06-01T12:00:00Z",
			Newest:  "2024-06-03T12:00:00Z",
		},
	}, nil
}

func (m *mockStore) CacheStats() (int, int) { return 0, 0 }

func (m *mockStore) WriteLog(entry db.LogEntry) error { return nil }

func (m *mockStore) GetLogs(opts db.LogQuery) ([]db.LogEntry, int, error) {
	return nil, 0, nil
}

func (m *mockStore) TrimLogs(maxEntries int) error    { return nil }
func (m *mockStore) TrimLogsByAge(maxAgeDays int) error { return nil }

func (m *mockStore) QueryTable(table string, limit, offset int) (*db.TableQueryResult, error) {
	return &db.TableQueryResult{Table: table, Limit: limit, Offset: offset}, nil
}

// ── Test Helpers ──────────────────────────────────────────────────────────────

func mustTime(s string) *time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return &t
}

func sampleFeeds() []db.Feed {
	return []db.Feed{
		{
			FeedID:      "feed-1",
			Title:       "Test Feed 1",
			URL:         "https://example.com/rss",
			UnreadCount: 5,
			FetchTier:   "active",
		},
		{
			FeedID:      "feed-2",
			Title:       "Test Feed 2",
			URL:         "https://other.com/rss",
			UnreadCount: 0,
			FetchTier:   "slowing",
		},
	}
}

func sampleArticles() []db.Article {
	return []db.Article{
		{
			ArticleID:   "art-1",
			FeedID:      "feed-1",
			Title:       "Article One",
			URL:         "https://example.com/1",
			Content:     "<p>Content 1</p>",
			Summary:     "Summary 1",
			PublishedAt: mustTime("2024-06-01T12:00:00Z"),
			IsRead:      false,
			IsStarred:   false,
		},
		{
			ArticleID:   "art-2",
			FeedID:      "feed-1",
			Title:       "Article Two",
			URL:         "https://example.com/2",
			Content:     "<p>Content 2</p>",
			Summary:     "Summary 2",
			PublishedAt: mustTime("2024-06-02T12:00:00Z"),
			IsRead:      true,
			IsStarred:   true,
		},
		{
			ArticleID:   "art-3",
			FeedID:      "feed-2",
			Title:       "Article Three",
			URL:         "https://other.com/3",
			Content:     "<p>Content 3</p>",
			Summary:     "Summary 3",
			PublishedAt: mustTime("2024-06-03T12:00:00Z"),
			IsRead:      false,
			IsStarred:   false,
		},
	}
}

// ── Feeds Handler Tests ───────────────────────────────────────────────────────

func TestListFeeds(t *testing.T) {
	store := newMockStore()
	store.feeds = sampleFeeds()
	h := NewFeedsHandler(store)

	req := httptest.NewRequest("GET", "/api/feeds", nil)
	w := httptest.NewRecorder()
	h.Handle(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var feeds []db.Feed
	if err := json.Unmarshal(w.Body.Bytes(), &feeds); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if len(feeds) != 2 {
		t.Fatalf("expected 2 feeds, got %d", len(feeds))
	}
	if feeds[0].Title != "Test Feed 1" {
		t.Errorf("expected 'Test Feed 1', got '%s'", feeds[0].Title)
	}
}

func TestListFeedsEmpty(t *testing.T) {
	store := newMockStore()
	h := NewFeedsHandler(store)

	req := httptest.NewRequest("GET", "/api/feeds", nil)
	w := httptest.NewRecorder()
	h.Handle(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var feeds []db.Feed
	json.Unmarshal(w.Body.Bytes(), &feeds)
	if len(feeds) != 0 {
		t.Errorf("expected empty array, got %d feeds", len(feeds))
	}
}

func TestGetFeed(t *testing.T) {
	store := newMockStore()
	store.feeds = sampleFeeds()
	h := NewFeedsHandler(store)

	req := httptest.NewRequest("GET", "/api/feeds/feed-1", nil)
	w := httptest.NewRecorder()
	h.Handle(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var feed db.Feed
	json.Unmarshal(w.Body.Bytes(), &feed)
	if feed.FeedID != "feed-1" {
		t.Errorf("expected feed-1, got %s", feed.FeedID)
	}
}

func TestGetFeedNotFound(t *testing.T) {
	store := newMockStore()
	h := NewFeedsHandler(store)

	req := httptest.NewRequest("GET", "/api/feeds/nonexistent", nil)
	w := httptest.NewRecorder()
	h.Handle(w, req)

	if w.Code != 404 {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestAddFeed(t *testing.T) {
	store := newMockStore()
	h := NewFeedsHandler(store)

	body := `{"url": "https://new-feed.com/rss"}`
	req := httptest.NewRequest("POST", "/api/feeds", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.Handle(w, req)

	if w.Code != 202 {
		t.Fatalf("expected 202, got %d", w.Code)
	}
	if store.lastFeedURL != "https://new-feed.com/rss" {
		t.Errorf("expected queued URL, got %s", store.lastFeedURL)
	}
}

func TestAddFeedMissingURL(t *testing.T) {
	store := newMockStore()
	h := NewFeedsHandler(store)

	body := `{"url": ""}`
	req := httptest.NewRequest("POST", "/api/feeds", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.Handle(w, req)

	if w.Code != 400 {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestAddFeedInvalidJSON(t *testing.T) {
	store := newMockStore()
	h := NewFeedsHandler(store)

	req := httptest.NewRequest("POST", "/api/feeds", strings.NewReader("not json"))
	w := httptest.NewRecorder()
	h.Handle(w, req)

	if w.Code != 400 {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestDeleteFeed501(t *testing.T) {
	store := newMockStore()
	h := NewFeedsHandler(store)

	req := httptest.NewRequest("DELETE", "/api/feeds/feed-1", nil)
	w := httptest.NewRecorder()
	h.Handle(w, req)

	if w.Code != 501 {
		t.Fatalf("expected 501, got %d", w.Code)
	}
}

func TestFeedArticles(t *testing.T) {
	store := newMockStore()
	store.articles = sampleArticles()
	h := NewFeedsHandler(store)

	req := httptest.NewRequest("GET", "/api/feeds/feed-1/articles", nil)
	w := httptest.NewRecorder()
	h.Handle(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var arts []db.Article
	json.Unmarshal(w.Body.Bytes(), &arts)
	if len(arts) != 2 {
		t.Fatalf("expected 2 articles for feed-1, got %d", len(arts))
	}
}

func TestFeedArticlesUnreadOnly(t *testing.T) {
	store := newMockStore()
	store.articles = sampleArticles()
	h := NewFeedsHandler(store)

	req := httptest.NewRequest("GET", "/api/feeds/feed-1/articles?unread=true", nil)
	w := httptest.NewRecorder()
	h.Handle(w, req)

	var arts []db.Article
	json.Unmarshal(w.Body.Bytes(), &arts)
	if len(arts) != 1 {
		t.Fatalf("expected 1 unread article for feed-1, got %d", len(arts))
	}
}

func TestFeedArticlesPagination(t *testing.T) {
	store := newMockStore()
	store.articles = sampleArticles()
	h := NewFeedsHandler(store)

	req := httptest.NewRequest("GET", "/api/feeds/feed-1/articles?limit=1&offset=0", nil)
	w := httptest.NewRecorder()
	h.Handle(w, req)

	var arts []db.Article
	json.Unmarshal(w.Body.Bytes(), &arts)
	if len(arts) != 1 {
		t.Fatalf("expected 1 article with limit=1, got %d", len(arts))
	}
}

func TestMarkAllRead(t *testing.T) {
	store := newMockStore()
	store.articles = sampleArticles()
	h := NewFeedsHandler(store)

	req := httptest.NewRequest("POST", "/api/feeds/feed-1/mark-all-read", nil)
	w := httptest.NewRecorder()
	h.Handle(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if store.writeCalls["MarkAllRead"] != 1 {
		t.Error("expected MarkAllRead to be called")
	}
}

func TestFeedsNotFound(t *testing.T) {
	store := newMockStore()
	h := NewFeedsHandler(store)

	req := httptest.NewRequest("PATCH", "/api/feeds/feed-1", nil)
	w := httptest.NewRecorder()
	h.Handle(w, req)

	if w.Code != 404 {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

// ── Articles Handler Tests ────────────────────────────────────────────────────

func TestListAllArticles(t *testing.T) {
	store := newMockStore()
	store.articles = sampleArticles()
	h := NewArticlesHandler(store)

	req := httptest.NewRequest("GET", "/api/articles/", nil)
	w := httptest.NewRecorder()
	h.Handle(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var arts []db.Article
	json.Unmarshal(w.Body.Bytes(), &arts)
	if len(arts) != 3 {
		t.Fatalf("expected 3 articles, got %d", len(arts))
	}
}

func TestListAllArticlesMethodNotAllowed(t *testing.T) {
	store := newMockStore()
	h := NewArticlesHandler(store)

	req := httptest.NewRequest("POST", "/api/articles/", nil)
	w := httptest.NewRecorder()
	h.Handle(w, req)

	if w.Code != 405 {
		t.Fatalf("expected 405, got %d", w.Code)
	}
}

func TestGetArticle(t *testing.T) {
	store := newMockStore()
	store.articles = sampleArticles()
	h := NewArticlesHandler(store)

	req := httptest.NewRequest("GET", "/api/articles/art-1", nil)
	w := httptest.NewRecorder()
	h.Handle(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var art db.Article
	json.Unmarshal(w.Body.Bytes(), &art)
	if art.ArticleID != "art-1" {
		t.Errorf("expected art-1, got %s", art.ArticleID)
	}
	if art.Content != "<p>Content 1</p>" {
		t.Errorf("expected content, got '%s'", art.Content)
	}
}

func TestGetArticleNotFound(t *testing.T) {
	store := newMockStore()
	h := NewArticlesHandler(store)

	req := httptest.NewRequest("GET", "/api/articles/nonexistent", nil)
	w := httptest.NewRecorder()
	h.Handle(w, req)

	if w.Code != 404 {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestMarkArticleRead(t *testing.T) {
	store := newMockStore()
	store.articles = sampleArticles()
	h := NewArticlesHandler(store)

	req := httptest.NewRequest("POST", "/api/articles/art-1/read", nil)
	w := httptest.NewRecorder()
	h.Handle(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if store.lastReadID != "art-1" {
		t.Errorf("expected art-1, got %s", store.lastReadID)
	}
}

func TestMarkArticleUnread(t *testing.T) {
	store := newMockStore()
	store.articles = sampleArticles()
	h := NewArticlesHandler(store)

	req := httptest.NewRequest("POST", "/api/articles/art-2/unread", nil)
	w := httptest.NewRecorder()
	h.Handle(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestStarArticle(t *testing.T) {
	store := newMockStore()
	store.articles = sampleArticles()
	h := NewArticlesHandler(store)

	req := httptest.NewRequest("POST", "/api/articles/art-1/star", nil)
	w := httptest.NewRecorder()
	h.Handle(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if store.lastStarred == nil || !*store.lastStarred {
		t.Error("expected starred=true")
	}
}

func TestUnstarArticle(t *testing.T) {
	store := newMockStore()
	store.articles = sampleArticles()
	h := NewArticlesHandler(store)

	req := httptest.NewRequest("POST", "/api/articles/art-2/unstar", nil)
	w := httptest.NewRecorder()
	h.Handle(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if store.lastStarred == nil || *store.lastStarred {
		t.Error("expected starred=false")
	}
}

func TestArticleNotFoundRoute(t *testing.T) {
	store := newMockStore()
	h := NewArticlesHandler(store)

	req := httptest.NewRequest("GET", "/api/articles/art-1/invalid", nil)
	w := httptest.NewRecorder()
	h.Handle(w, req)

	if w.Code != 404 {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

// ── Batch Fetch Tests ─────────────────────────────────────────────────────────

func TestBatchFetchArticles(t *testing.T) {
	store := newMockStore()
	store.articles = sampleArticles()
	h := NewArticlesHandler(store)

	body := `{"ids": ["art-1", "art-3"]}`
	req := httptest.NewRequest("POST", "/api/articles/batch", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.Handle(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var arts []db.Article
	if err := json.Unmarshal(w.Body.Bytes(), &arts); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if len(arts) != 2 {
		t.Fatalf("expected 2 articles, got %d", len(arts))
	}

	// Verify content is included
	for _, a := range arts {
		if a.Content == "" {
			t.Errorf("expected content for %s, got empty", a.ArticleID)
		}
	}

	if store.readCalls["GetArticleBatch"] != 1 {
		t.Errorf("expected 1 GetArticleBatch call, got %d", store.readCalls["GetArticleBatch"])
	}
}

func TestBatchFetchEmpty(t *testing.T) {
	store := newMockStore()
	h := NewArticlesHandler(store)

	body := `{"ids": []}`
	req := httptest.NewRequest("POST", "/api/articles/batch", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.Handle(w, req)

	if w.Code != 400 {
		t.Fatalf("expected 400 for empty ids, got %d", w.Code)
	}
}

func TestBatchFetchInvalidJSON(t *testing.T) {
	store := newMockStore()
	h := NewArticlesHandler(store)

	req := httptest.NewRequest("POST", "/api/articles/batch", strings.NewReader("not json"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.Handle(w, req)

	if w.Code != 400 {
		t.Fatalf("expected 400 for invalid JSON, got %d", w.Code)
	}
}

func TestBatchFetchNonExistent(t *testing.T) {
	store := newMockStore()
	store.articles = sampleArticles()
	h := NewArticlesHandler(store)

	body := `{"ids": ["nonexistent-1", "nonexistent-2"]}`
	req := httptest.NewRequest("POST", "/api/articles/batch", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.Handle(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var arts []db.Article
	json.Unmarshal(w.Body.Bytes(), &arts)
	if len(arts) != 0 {
		t.Errorf("expected 0 articles for non-existent IDs, got %d", len(arts))
	}
}

// ── Categories Handler Tests ──────────────────────────────────────────────────

func TestListCategories(t *testing.T) {
	store := newMockStore()
	store.categories = []db.Category{
		{CategoryID: "cat-1", Name: "Tech", SortOrder: 1},
		{CategoryID: "cat-2", Name: "News", SortOrder: 2},
	}
	h := NewCategoriesHandler(store)

	req := httptest.NewRequest("GET", "/api/categories", nil)
	w := httptest.NewRecorder()
	h.Handle(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var cats []db.Category
	json.Unmarshal(w.Body.Bytes(), &cats)
	if len(cats) != 2 {
		t.Fatalf("expected 2 categories, got %d", len(cats))
	}
}

func TestListCategoriesEmpty(t *testing.T) {
	store := newMockStore()
	h := NewCategoriesHandler(store)

	req := httptest.NewRequest("GET", "/api/categories", nil)
	w := httptest.NewRecorder()
	h.Handle(w, req)

	var cats []db.Category
	json.Unmarshal(w.Body.Bytes(), &cats)
	if len(cats) != 0 {
		t.Errorf("expected empty array, got %d", len(cats))
	}
}

func TestCategoriesMethodNotAllowed(t *testing.T) {
	store := newMockStore()
	h := NewCategoriesHandler(store)

	req := httptest.NewRequest("POST", "/api/categories", nil)
	w := httptest.NewRecorder()
	h.Handle(w, req)

	if w.Code != 405 {
		t.Fatalf("expected 405, got %d", w.Code)
	}
}

// ── Helpers Tests ─────────────────────────────────────────────────────────────

func TestQueryInt(t *testing.T) {
	tests := []struct {
		input    string
		def      int
		expected int
	}{
		{"", 50, 50},
		{"10", 50, 10},
		{"0", 50, 0},
		{"-1", 50, 50},
		{"abc", 50, 50},
		{"100", 25, 100},
	}
	for _, tc := range tests {
		result := queryInt(tc.input, tc.def)
		if result != tc.expected {
			t.Errorf("queryInt(%q, %d) = %d, want %d", tc.input, tc.def, result, tc.expected)
		}
	}
}

// ── Content-Type Tests ────────────────────────────────────────────────────────

func TestResponseContentType(t *testing.T) {
	store := newMockStore()
	store.feeds = sampleFeeds()
	h := NewFeedsHandler(store)

	req := httptest.NewRequest("GET", "/api/feeds", nil)
	w := httptest.NewRecorder()
	h.Handle(w, req)

	ct := w.Header().Get("Content-Type")
	if !strings.Contains(ct, "application/json") {
		t.Errorf("expected application/json, got %s", ct)
	}
}

// ── Status Handler Tests ──────────────────────────────────────────────────────

func TestGetStatus(t *testing.T) {
	store := newMockStore()
	store.articles = sampleArticles()
	store.feeds = sampleFeeds()
	h := NewStatusHandler(store)

	req := httptest.NewRequest("GET", "/api/status", nil)
	w := httptest.NewRecorder()
	h.Handle(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var status db.DBStatus
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	// 3 articles in sample data
	if status.Articles.Total != 3 {
		t.Errorf("expected 3 total articles, got %d", status.Articles.Total)
	}
	// art-1 and art-3 are unread
	if status.Articles.Unread != 2 {
		t.Errorf("expected 2 unread, got %d", status.Articles.Unread)
	}
	// art-2 is starred
	if status.Articles.Starred != 1 {
		t.Errorf("expected 1 starred, got %d", status.Articles.Starred)
	}

	// Should have table stats
	if len(status.Tables) < 2 {
		t.Errorf("expected at least 2 tables, got %d", len(status.Tables))
	}

	// articles table should report 3 rows
	for _, ts := range status.Tables {
		if ts.Name == "articles" {
			if ts.RowCount != 3 {
				t.Errorf("articles table row_count: expected 3, got %d", ts.RowCount)
			}
		}
		if ts.Name == "feeds" {
			if ts.RowCount != 2 {
				t.Errorf("feeds table row_count: expected 2, got %d", ts.RowCount)
			}
		}
	}

	if status.DataPath == "" {
		t.Error("expected non-empty data_path")
	}
	if status.Articles.Oldest == "" {
		t.Error("expected non-empty oldest date")
	}
	if status.Articles.Newest == "" {
		t.Error("expected non-empty newest date")
	}
}

func TestGetStatusMethodNotAllowed(t *testing.T) {
	store := newMockStore()
	h := NewStatusHandler(store)

	req := httptest.NewRequest("POST", "/api/status", nil)
	w := httptest.NewRecorder()
	h.Handle(w, req)

	if w.Code != 405 {
		t.Fatalf("expected 405, got %d", w.Code)
	}
}

func TestGetStatusContentType(t *testing.T) {
	store := newMockStore()
	h := NewStatusHandler(store)

	req := httptest.NewRequest("GET", "/api/status", nil)
	w := httptest.NewRecorder()
	h.Handle(w, req)

	ct := w.Header().Get("Content-Type")
	if !strings.Contains(ct, "application/json") {
		t.Errorf("expected application/json, got %s", ct)
	}
}

// ── Server Status Tests ──────────────────────────────────────────────────────

func TestServerStatusSuccess(t *testing.T) {
	start := time.Now().Add(-10 * time.Second)
	h := NewServerStatusHandler(start, func() CacheStatsInfo {
		return CacheStatsInfo{PendingReads: 3, PendingStars: 1}
	})

	req := httptest.NewRequest("GET", "/api/server-status", nil)
	w := httptest.NewRecorder()
	h.Handle(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var body map[string]any
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode error: %v", err)
	}

	// Top-level keys
	for _, key := range []string{"server", "host", "memory", "gc", "write_cache"} {
		if _, ok := body[key]; !ok {
			t.Errorf("missing top-level key %q", key)
		}
	}

	// Server section
	srv, _ := body["server"].(map[string]any)
	if srv == nil {
		t.Fatal("server section is nil")
	}
	if v, ok := srv["uptime_seconds"].(float64); !ok || v < 10 {
		t.Errorf("expected uptime_seconds >= 10, got %v", srv["uptime_seconds"])
	}
	if srv["go_version"] == nil || srv["go_version"] == "" {
		t.Error("missing go_version")
	}
	if srv["goroutines"] == nil {
		t.Error("missing goroutines")
	}

	// Memory section
	mem, _ := body["memory"].(map[string]any)
	if mem == nil {
		t.Fatal("memory section is nil")
	}
	if _, ok := mem["heap_alloc_bytes"]; !ok {
		t.Error("missing heap_alloc_bytes")
	}
	if _, ok := mem["sys_bytes"]; !ok {
		t.Error("missing sys_bytes")
	}

	// GC section
	gc, _ := body["gc"].(map[string]any)
	if gc == nil {
		t.Fatal("gc section is nil")
	}
	if _, ok := gc["num_gc"]; !ok {
		t.Error("missing num_gc")
	}

	// Write cache
	wc, _ := body["write_cache"].(map[string]any)
	if wc == nil {
		t.Fatal("write_cache section is nil")
	}
	if v, _ := wc["pending_reads"].(float64); v != 3 {
		t.Errorf("expected pending_reads=3, got %v", wc["pending_reads"])
	}
	if v, _ := wc["pending_stars"].(float64); v != 1 {
		t.Errorf("expected pending_stars=1, got %v", wc["pending_stars"])
	}
}

func TestServerStatusMethodNotAllowed(t *testing.T) {
	h := NewServerStatusHandler(time.Now(), nil)

	req := httptest.NewRequest("POST", "/api/server-status", nil)
	w := httptest.NewRecorder()
	h.Handle(w, req)

	if w.Code != 405 {
		t.Fatalf("expected 405, got %d", w.Code)
	}
}

func TestServerStatusContentType(t *testing.T) {
	h := NewServerStatusHandler(time.Now(), nil)

	req := httptest.NewRequest("GET", "/api/server-status", nil)
	w := httptest.NewRecorder()
	h.Handle(w, req)

	ct := w.Header().Get("Content-Type")
	if !strings.Contains(ct, "application/json") {
		t.Errorf("expected application/json, got %s", ct)
	}
}

func TestServerStatusNilCacheStats(t *testing.T) {
	h := NewServerStatusHandler(time.Now(), nil)

	req := httptest.NewRequest("GET", "/api/server-status", nil)
	w := httptest.NewRecorder()
	h.Handle(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var body map[string]any
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode error: %v", err)
	}

	wc, _ := body["write_cache"].(map[string]any)
	if wc == nil {
		t.Fatal("write_cache section is nil")
	}
	if v, _ := wc["pending_reads"].(float64); v != 0 {
		t.Errorf("expected pending_reads=0, got %v", wc["pending_reads"])
	}
}

func TestGetStatusReflectsStateChanges(t *testing.T) {
	store := newMockStore()
	store.articles = sampleArticles()
	h := NewStatusHandler(store)

	// Initial state: 2 unread, 1 starred
	req := httptest.NewRequest("GET", "/api/status", nil)
	w := httptest.NewRecorder()
	h.Handle(w, req)

	var s1 db.DBStatus
	json.Unmarshal(w.Body.Bytes(), &s1)
	if s1.Articles.Unread != 2 {
		t.Fatalf("initial unread: expected 2, got %d", s1.Articles.Unread)
	}

	// Mark art-1 as read
	for i := range store.articles {
		if store.articles[i].ArticleID == "art-1" {
			store.articles[i].IsRead = true
		}
	}

	// Star art-3
	for i := range store.articles {
		if store.articles[i].ArticleID == "art-3" {
			store.articles[i].IsStarred = true
		}
	}

	req = httptest.NewRequest("GET", "/api/status", nil)
	w = httptest.NewRecorder()
	h.Handle(w, req)

	var s2 db.DBStatus
	json.Unmarshal(w.Body.Bytes(), &s2)

	// Now only art-3 is unread (art-1 read, art-2 was already read)
	if s2.Articles.Unread != 1 {
		t.Errorf("after mark-read: expected 1 unread, got %d", s2.Articles.Unread)
	}
	// art-2 + art-3 starred
	if s2.Articles.Starred != 2 {
		t.Errorf("after star: expected 2 starred, got %d", s2.Articles.Starred)
	}
	if s2.Articles.Total != 3 {
		t.Errorf("total should stay 3, got %d", s2.Articles.Total)
	}
}
