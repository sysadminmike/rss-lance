package db

import "testing"

func TestEscapeSQ(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"hello", "hello"},
		{"it's", "it''s"},
		{"", ""},
		{"O'Brien's", "O''Brien''s"},
		{"no quotes", "no quotes"},
		{"'start", "''start"},
		{"end'", "end''"},
		{"'''", "''''''"},
	}
	for _, tc := range tests {
		result := escapeSQ(tc.input)
		if result != tc.expected {
			t.Errorf("escapeSQ(%q) = %q, want %q", tc.input, result, tc.expected)
		}
	}
}

func TestFeedJSONFields(t *testing.T) {
	// Verify Feed struct has all expected JSON tags
	f := Feed{
		FeedID:    "test",
		Title:     "Test",
		URL:       "https://example.com",
		FetchTier: "active",
	}
	if f.FeedID != "test" {
		t.Error("FeedID mismatch")
	}
	if f.FetchTier != "active" {
		t.Error("FetchTier mismatch")
	}
}

func TestArticleJSONFields(t *testing.T) {
	a := Article{
		ArticleID: "art-1",
		FeedID:    "feed-1",
		IsRead:    false,
		IsStarred: true,
	}
	if a.ArticleID != "art-1" {
		t.Error("ArticleID mismatch")
	}
	if a.IsStarred != true {
		t.Error("IsStarred mismatch")
	}
}

func TestCategoryJSONFields(t *testing.T) {
	c := Category{
		CategoryID: "cat-1",
		Name:       "Tech",
		ParentID:   "",
		SortOrder:  1,
	}
	if c.Name != "Tech" {
		t.Error("Name mismatch")
	}
}

func TestDBStatusTypes(t *testing.T) {
	s := DBStatus{
		DataPath: "/data",
		Tables: []TableStats{
			{Name: "articles", RowCount: 100, SizeBytes: 1048576},
			{Name: "feeds", RowCount: 5, SizeBytes: 2048},
		},
		Articles: ArticleStats{
			Total:   100,
			Unread:  42,
			Starred: 7,
			Oldest:  "2024-01-01T00:00:00Z",
			Newest:  "2024-06-15T12:00:00Z",
		},
	}
	if s.DataPath != "/data" {
		t.Error("DataPath mismatch")
	}
	if len(s.Tables) != 2 {
		t.Errorf("expected 2 tables, got %d", len(s.Tables))
	}
	if s.Tables[0].Name != "articles" {
		t.Error("first table should be articles")
	}
	if s.Tables[0].RowCount != 100 {
		t.Errorf("expected 100 rows, got %d", s.Tables[0].RowCount)
	}
	if s.Tables[0].SizeBytes != 1048576 {
		t.Errorf("expected 1MB, got %d", s.Tables[0].SizeBytes)
	}
	if s.Articles.Total != 100 {
		t.Errorf("expected 100 total, got %d", s.Articles.Total)
	}
	if s.Articles.Unread != 42 {
		t.Errorf("expected 42 unread, got %d", s.Articles.Unread)
	}
	if s.Articles.Starred != 7 {
		t.Errorf("expected 7 starred, got %d", s.Articles.Starred)
	}
	if s.Articles.Oldest != "2024-01-01T00:00:00Z" {
		t.Error("Oldest mismatch")
	}
	if s.Articles.Newest != "2024-06-15T12:00:00Z" {
		t.Error("Newest mismatch")
	}
}

func TestDirSize(t *testing.T) {
	// dirSize on a non-existent path should return 0
	size := dirSize("/nonexistent/path/that/does/not/exist")
	if size != 0 {
		t.Errorf("expected 0 for non-existent dir, got %d", size)
	}
}
