//go:build windows

package db

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/apache/arrow/go/v17/arrow"
	"github.com/apache/arrow/go/v17/arrow/array"
	"github.com/apache/arrow/go/v17/arrow/memory"
	"github.com/lancedb/lancedb-go/pkg/lancedb"
)

// articlesSchema mirrors the Python fetcher's ARTICLES_SCHEMA.
var testArticlesSchema = arrow.NewSchema([]arrow.Field{
	{Name: "article_id", Type: arrow.BinaryTypes.String, Nullable: false},
	{Name: "feed_id", Type: arrow.BinaryTypes.String, Nullable: false},
	{Name: "title", Type: arrow.BinaryTypes.String, Nullable: true},
	{Name: "url", Type: arrow.BinaryTypes.String, Nullable: true},
	{Name: "author", Type: arrow.BinaryTypes.String, Nullable: true},
	{Name: "content", Type: arrow.BinaryTypes.String, Nullable: true},
	{Name: "summary", Type: arrow.BinaryTypes.String, Nullable: true},
	{Name: "published_at", Type: &arrow.TimestampType{Unit: arrow.Microsecond}, Nullable: true},
	{Name: "fetched_at", Type: &arrow.TimestampType{Unit: arrow.Microsecond}, Nullable: true},
	{Name: "is_read", Type: arrow.FixedWidthTypes.Boolean, Nullable: false},
	{Name: "is_starred", Type: arrow.FixedWidthTypes.Boolean, Nullable: false},
	{Name: "guid", Type: arrow.BinaryTypes.String, Nullable: true},
	{Name: "schema_version", Type: arrow.PrimitiveTypes.Int32, Nullable: true},
	{Name: "created_at", Type: &arrow.TimestampType{Unit: arrow.Microsecond}, Nullable: true},
	{Name: "updated_at", Type: &arrow.TimestampType{Unit: arrow.Microsecond}, Nullable: true},
}, nil)

// buildArticlesRecord creates an Arrow record with the given article data.
func buildArticlesRecord(pool memory.Allocator, articles []testArticle) arrow.Record {
	n := len(articles)

	articleIDBuilder := array.NewStringBuilder(pool)
	feedIDBuilder := array.NewStringBuilder(pool)
	titleBuilder := array.NewStringBuilder(pool)
	urlBuilder := array.NewStringBuilder(pool)
	authorBuilder := array.NewStringBuilder(pool)
	contentBuilder := array.NewStringBuilder(pool)
	summaryBuilder := array.NewStringBuilder(pool)
	publishedAtBuilder := array.NewTimestampBuilder(pool, &arrow.TimestampType{Unit: arrow.Microsecond})
	fetchedAtBuilder := array.NewTimestampBuilder(pool, &arrow.TimestampType{Unit: arrow.Microsecond})
	isReadBuilder := array.NewBooleanBuilder(pool)
	isStarredBuilder := array.NewBooleanBuilder(pool)
	guidBuilder := array.NewStringBuilder(pool)
	schemaVersionBuilder := array.NewInt32Builder(pool)
	createdAtBuilder := array.NewTimestampBuilder(pool, &arrow.TimestampType{Unit: arrow.Microsecond})
	updatedAtBuilder := array.NewTimestampBuilder(pool, &arrow.TimestampType{Unit: arrow.Microsecond})

	now := arrow.Timestamp(time.Now().UTC().UnixMicro())

	for _, a := range articles {
		articleIDBuilder.Append(a.ID)
		feedIDBuilder.Append(a.FeedID)
		titleBuilder.Append(a.Title)
		urlBuilder.Append("https://example.com/" + a.ID)
		authorBuilder.Append("test-author")
		contentBuilder.Append("test content")
		summaryBuilder.Append("test summary")
		publishedAtBuilder.Append(now)
		fetchedAtBuilder.Append(now)
		isReadBuilder.Append(a.IsRead)
		isStarredBuilder.Append(a.IsStarred)
		guidBuilder.Append(a.ID)
		schemaVersionBuilder.Append(1)
		createdAtBuilder.Append(now)
		updatedAtBuilder.Append(now)
	}

	cols := []arrow.Array{
		articleIDBuilder.NewArray(),
		feedIDBuilder.NewArray(),
		titleBuilder.NewArray(),
		urlBuilder.NewArray(),
		authorBuilder.NewArray(),
		contentBuilder.NewArray(),
		summaryBuilder.NewArray(),
		publishedAtBuilder.NewArray(),
		fetchedAtBuilder.NewArray(),
		isReadBuilder.NewArray(),
		isStarredBuilder.NewArray(),
		guidBuilder.NewArray(),
		schemaVersionBuilder.NewArray(),
		createdAtBuilder.NewArray(),
		updatedAtBuilder.NewArray(),
	}

	record := array.NewRecord(testArticlesSchema, cols, int64(n))

	// Release individual arrays (record holds references)
	for _, col := range cols {
		col.Release()
	}

	return record
}

type testArticle struct {
	ID        string
	FeedID    string
	Title     string
	IsRead    bool
	IsStarred bool
}

// findDuckDBForTest locates duckdb.exe, also checking project-relative paths
// since "go test" CWD is the package directory (server/db/).
func findDuckDBForTest() (string, error) {
	// Try the normal search first
	if bin, err := findDuckDB(); err == nil {
		return bin, nil
	}
	// Check paths relative to the package dir (go test runs from server/db/)
	for _, rel := range []string{
		`..\..\tools\duckdb.exe`, // project root tools/
		`..\tools\duckdb.exe`,
	} {
		if _, err := os.Stat(rel); err == nil {
			abs, _ := filepath.Abs(rel)
			return abs, nil
		}
	}
	return "", fmt.Errorf("duckdb.exe not found for testing")
}

// TestGetDBStatusArticleMetrics is an integration test that:
// 1. Creates a temporary Lance dataset with articles of known read/starred state
// 2. Opens a cliStore against it
// 3. Calls GetDBStatus and verifies the article aggregate stats
//
// Requires duckdb.exe to be findable (in tools/, next to binary, or PATH).
func TestGetDBStatusArticleMetrics(t *testing.T) {
	// Check duckdb.exe is available
	duckdbBin, err := findDuckDBForTest()
	if err != nil {
		t.Skipf("Skipping integration test: %v", err)
	}
	t.Logf("Found duckdb.exe: %s", duckdbBin)

	// Create temp data directory
	tmpDir, err := os.MkdirTemp("", "rss-lance-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Connect via lancedb-go to create the articles table
	ctx := context.Background()
	conn, err := lancedb.Connect(ctx, tmpDir, nil)
	if err != nil {
		t.Fatalf("Failed to connect lancedb: %v", err)
	}

	schema, err := lancedb.NewSchema(testArticlesSchema)
	if err != nil {
		t.Fatalf("Failed to create schema: %v", err)
	}

	tbl, err := conn.CreateTable(ctx, "articles", schema)
	if err != nil {
		t.Fatalf("Failed to create articles table: %v", err)
	}

	// Insert test articles: 3 unread, 2 read, 1 starred
	pool := memory.NewGoAllocator()
	articles := []testArticle{
		{ID: "art-1", FeedID: "feed-1", Title: "Unread 1", IsRead: false, IsStarred: false},
		{ID: "art-2", FeedID: "feed-1", Title: "Unread 2", IsRead: false, IsStarred: false},
		{ID: "art-3", FeedID: "feed-1", Title: "Unread 3 Starred", IsRead: false, IsStarred: true},
		{ID: "art-4", FeedID: "feed-2", Title: "Read 1", IsRead: true, IsStarred: false},
		{ID: "art-5", FeedID: "feed-2", Title: "Read 2 Starred", IsRead: true, IsStarred: true},
	}

	record := buildArticlesRecord(pool, articles)
	defer record.Release()

	err = tbl.AddRecords(ctx, []arrow.Record{record}, nil)
	if err != nil {
		t.Fatalf("Failed to add articles: %v", err)
	}

	count, err := tbl.Count(ctx)
	if err != nil {
		t.Fatalf("Failed to count: %v", err)
	}
	t.Logf("Articles table has %d rows", count)

	// Close lancedb-go resources before DuckDB reads
	tbl.Close()
	conn.Close()

	// Now create a cliStore and run GetDBStatus
	dbPath := filepath.Join(tmpDir, "server.duckdb")
	s := &cliStore{
		dataPath:  tmpDir,
		dbPath:    dbPath,
		duckdbBin: duckdbBin,
	}

	// Bootstrap: install lance extension
	if err := s.bootstrap(); err != nil {
		t.Fatalf("Bootstrap failed: %v", err)
	}

	// Start persistent DuckDB process (Phase 2)
	proc, err := newDuckDBProcess(duckdbBin, dbPath, tmpDir)
	if err != nil {
		t.Fatalf("Failed to start persistent DuckDB process: %v", err)
	}
	defer proc.close()
	s.duckProc = proc

	// Set up a writer (needed for getTableMeta) and a no-op cache
	w, err := newLanceWriter(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create lance writer: %v", err)
	}
	defer w.close()
	s.writer = w

	// No-op write cache (no pending overrides)
	s.cache = newWriteCache()
	defer s.cache.close()

	// No-op log buffer
	s.logBuf = newLogBuffer(LogBufferConfig{
		FlushThreshold:    100,
		FlushIntervalSecs: 3600,
	}, func(entries []LogEntry) error {
		return nil
	})
	defer s.logBuf.close()

	// ── Test 1: raw query to inspect what DuckDB sees ──
	t.Run("RawQuery_TypeCheck", func(t *testing.T) {
		q := fmt.Sprintf("SELECT is_read, typeof(is_read) AS read_type, is_starred, typeof(is_starred) AS star_type FROM %s LIMIT 5",
			s.lanceTable("articles"))
		rows, err := s.lanceQuery(q)
		if err != nil {
			t.Fatalf("Type check query failed: %v", err)
		}
		for i, row := range rows {
			t.Logf("Row %d: is_read=%v (type=%v), is_starred=%v (type=%v)",
				i, row["is_read"], row["read_type"], row["is_starred"], row["star_type"])
		}
	})

	// ── Test 2: simple COUNT to verify DuckDB can see articles ──
	t.Run("RawQuery_Count", func(t *testing.T) {
		q := fmt.Sprintf("SELECT COUNT(*) AS cnt FROM %s", s.lanceTable("articles"))
		rows, err := s.lanceQuery(q)
		if err != nil {
			t.Fatalf("Count query failed: %v", err)
		}
		if len(rows) == 0 {
			t.Fatal("Count query returned no rows")
		}
		total := rowInt(rows[0], "cnt")
		t.Logf("COUNT(*) = %d", total)
		if total != 5 {
			t.Errorf("Expected 5 articles, got %d", total)
		}
	})

	// ── Test 3: the exact aggregation query from GetDBStatus (no ::BOOLEAN) ──
	t.Run("RawQuery_ArticleStats", func(t *testing.T) {
		q := fmt.Sprintf(`SELECT
			COUNT(*) AS total,
			SUM(CASE WHEN a.is_read = false THEN 1 ELSE 0 END) AS unread,
			SUM(CASE WHEN a.is_starred = true THEN 1 ELSE 0 END) AS starred
		FROM %s a`, s.lanceTable("articles"))
		rows, err := s.lanceQuery(q)
		if err != nil {
			t.Fatalf("Article stats query failed: %v", err)
		}
		if len(rows) == 0 {
			t.Fatal("Article stats query returned no rows")
		}
		t.Logf("Raw result: %+v", rows[0])

		total := rowInt(rows[0], "total")
		unread := rowInt(rows[0], "unread")
		starred := rowInt(rows[0], "starred")

		t.Logf("total=%d unread=%d starred=%d", total, unread, starred)

		if total != 5 {
			t.Errorf("Total: got %d, want 5", total)
		}
		if unread != 3 {
			t.Errorf("Unread: got %d, want 3", unread)
		}
		if starred != 2 {
			t.Errorf("Starred: got %d, want 2", starred)
		}
	})

	// ── Test 4: alternative queries to help diagnose issues ──
	t.Run("RawQuery_NoCast", func(t *testing.T) {
		q := fmt.Sprintf(`SELECT
			COUNT(*) AS total,
			SUM(CASE WHEN a.is_read = false THEN 1 ELSE 0 END) AS unread,
			SUM(CASE WHEN a.is_starred = true THEN 1 ELSE 0 END) AS starred
		FROM %s a`, s.lanceTable("articles"))
		rows, err := s.lanceQuery(q)
		if err != nil {
			t.Fatalf("No-cast query failed: %v", err)
		}
		if len(rows) == 0 {
			t.Fatal("No-cast query returned no rows")
		}
		t.Logf("[no cast] total=%v unread=%v starred=%v", rows[0]["total"], rows[0]["unread"], rows[0]["starred"])
	})

	t.Run("RawQuery_CountWhere", func(t *testing.T) {
		q := fmt.Sprintf(`SELECT
			COUNT(*) AS total,
			COUNT(*) FILTER (WHERE NOT a.is_read::BOOLEAN) AS unread,
			COUNT(*) FILTER (WHERE a.is_starred::BOOLEAN) AS starred
		FROM %s a`, s.lanceTable("articles"))
		rows, err := s.lanceQuery(q)
		if err != nil {
			t.Fatalf("FILTER query failed: %v", err)
		}
		if len(rows) == 0 {
			t.Fatal("FILTER query returned no rows")
		}
		t.Logf("[FILTER] total=%v unread=%v starred=%v", rows[0]["total"], rows[0]["unread"], rows[0]["starred"])
	})

	// ── Test 5: GetDBStatus end-to-end ──
	t.Run("GetDBStatus", func(t *testing.T) {
		status, err := s.GetDBStatus()
		if err != nil {
			t.Fatalf("GetDBStatus failed: %v", err)
		}

		t.Logf("GetDBStatus result: total=%d unread=%d starred=%d oldest=%q newest=%q",
			status.Articles.Total, status.Articles.Unread, status.Articles.Starred,
			status.Articles.Oldest, status.Articles.Newest)

		if status.Articles.Total != 5 {
			t.Errorf("Total: got %d, want 5", status.Articles.Total)
		}
		if status.Articles.Unread != 3 {
			t.Errorf("Unread: got %d, want 3", status.Articles.Unread)
		}
		if status.Articles.Starred != 2 {
			t.Errorf("Starred: got %d, want 2", status.Articles.Starred)
		}

		// Verify table stats
		var artTable *TableStats
		for i := range status.Tables {
			if status.Tables[i].Name == "articles" {
				artTable = &status.Tables[i]
				break
			}
		}
		if artTable == nil {
			t.Fatal("articles table not found in status.Tables")
		}
		if artTable.RowCount != 5 {
			t.Errorf("Table row count: got %d, want 5", artTable.RowCount)
		}
	})

	// ── Test 6: test with write cache pending overrides ──
	t.Run("GetDBStatus_WithCache", func(t *testing.T) {
		// Mark art-4 as unread via cache (override the stored is_read=true)
		s.cache.setRead("art-4", false)
		// Mark art-1 as starred via cache (override the stored is_starred=false)
		s.cache.setStarred("art-1", true)

		status, err := s.GetDBStatus()
		if err != nil {
			t.Fatalf("GetDBStatus with cache failed: %v", err)
		}

		t.Logf("GetDBStatus with cache: total=%d unread=%d starred=%d",
			status.Articles.Total, status.Articles.Unread, status.Articles.Starred)

		// Now: unread should be 4 (art-1,2,3 from table + art-4 from cache override)
		// Starred should be 3 (art-3,5 from table + art-1 from cache override)
		if status.Articles.Total != 5 {
			t.Errorf("Total with cache: got %d, want 5", status.Articles.Total)
		}
		if status.Articles.Unread != 4 {
			t.Errorf("Unread with cache: got %d, want 4", status.Articles.Unread)
		}
		if status.Articles.Starred != 3 {
			t.Errorf("Starred with cache: got %d, want 3", status.Articles.Starred)
		}
	})
}
