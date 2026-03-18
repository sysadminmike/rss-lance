//go:build windows

package db

import (
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// findTestDuckDB locates duckdb.exe for tests.
// Looks in tools/ relative to the repo root.
func findTestDuckDB(t *testing.T) string {
	t.Helper()
	// Walk up from the test directory to find the repo root
	dir, _ := os.Getwd()
	for i := 0; i < 5; i++ {
		candidate := filepath.Join(dir, "tools", "duckdb.exe")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		dir = filepath.Dir(dir)
	}
	// Also try findDuckDB which checks PATH
	if p, err := findDuckDB(); err == nil {
		return p
	}
	t.Skip("duckdb.exe not found - skipping persistent process tests")
	return ""
}

// testDataPath returns a path to the real data/ directory if it exists,
// or empty string if not available. Tests that need Lance tables should
// skip if this returns empty.
func testDataPath(t *testing.T) string {
	t.Helper()
	dir, _ := os.Getwd()
	for i := 0; i < 5; i++ {
		candidate := filepath.Join(dir, "data")
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			// Check that at least feeds.lance exists
			if _, err := os.Stat(filepath.Join(candidate, "feeds.lance")); err == nil {
				return candidate
			}
		}
		dir = filepath.Dir(dir)
	}
	return ""
}

// testDBPath returns the path for a test server.duckdb file.
func testDBPath(t *testing.T) string {
	t.Helper()
	dataPath := testDataPath(t)
	if dataPath == "" {
		return filepath.Join(t.TempDir(), "test.duckdb")
	}
	return filepath.Join(dataPath, "server.duckdb")
}

func TestDuckDBProcessStartup(t *testing.T) {
	duckdbBin := findTestDuckDB(t)
	dataPath := testDataPath(t)
	if dataPath == "" {
		t.Skip("data/ directory with Lance tables not found")
	}
	dbPath := filepath.Join(dataPath, "server.duckdb")

	// Ensure extension is installed (Phase 1)
	cmd := exec.Command(duckdbBin, dbPath, "-c", "INSTALL lance FROM community;")
	cmd.CombinedOutput()

	// Phase 2: start persistent process
	proc, err := newDuckDBProcess(duckdbBin, dbPath, dataPath)
	if err != nil {
		t.Fatalf("newDuckDBProcess failed: %v", err)
	}
	defer proc.close()

	if !proc.alive {
		t.Fatal("process should be alive after start")
	}
	if proc.pid() == 0 {
		t.Fatal("process PID should be non-zero")
	}
}

func TestDuckDBProcessQuery(t *testing.T) {
	duckdbBin := findTestDuckDB(t)
	dataPath := testDataPath(t)
	if dataPath == "" {
		t.Skip("data/ directory with Lance tables not found")
	}
	dbPath := filepath.Join(dataPath, "server.duckdb")

	cmd := exec.Command(duckdbBin, dbPath, "-c", "INSTALL lance FROM community;")
	cmd.CombinedOutput()

	proc, err := newDuckDBProcess(duckdbBin, dbPath, dataPath)
	if err != nil {
		t.Fatalf("newDuckDBProcess failed: %v", err)
	}
	defer proc.close()

	// Basic query: 1+1
	rows, err := proc.query("SELECT 1+1 AS result;")
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	// DuckDB JSON returns numbers as float64
	val, ok := rows[0]["result"]
	if !ok {
		t.Fatal("missing 'result' key in row")
	}
	if v, ok := val.(float64); !ok || v != 2 {
		t.Fatalf("expected result=2, got %v (type %T)", val, val)
	}
}

func TestDuckDBProcessLanceQuery(t *testing.T) {
	duckdbBin := findTestDuckDB(t)
	dataPath := testDataPath(t)
	if dataPath == "" {
		t.Skip("data/ directory with Lance tables not found")
	}
	dbPath := filepath.Join(dataPath, "server.duckdb")

	cmd := exec.Command(duckdbBin, dbPath, "-c", "INSTALL lance FROM community;")
	cmd.CombinedOutput()

	proc, err := newDuckDBProcess(duckdbBin, dbPath, dataPath)
	if err != nil {
		t.Fatalf("newDuckDBProcess failed: %v", err)
	}
	defer proc.close()

	// Query a Lance table
	rows, err := proc.query("SELECT COUNT(*) AS cnt FROM _lance.main.feeds;")
	if err != nil {
		t.Fatalf("lance query failed: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	cnt, ok := rows[0]["cnt"]
	if !ok {
		t.Fatal("missing 'cnt' key")
	}
	t.Logf("Feed count: %v", cnt)
}

func TestDuckDBProcessEmptyResult(t *testing.T) {
	duckdbBin := findTestDuckDB(t)
	dataPath := testDataPath(t)
	if dataPath == "" {
		t.Skip("data/ directory with Lance tables not found")
	}
	dbPath := filepath.Join(dataPath, "server.duckdb")

	cmd := exec.Command(duckdbBin, dbPath, "-c", "INSTALL lance FROM community;")
	cmd.CombinedOutput()

	proc, err := newDuckDBProcess(duckdbBin, dbPath, dataPath)
	if err != nil {
		t.Fatalf("newDuckDBProcess failed: %v", err)
	}
	defer proc.close()

	// Query that returns no rows
	rows, err := proc.query("SELECT * FROM _lance.main.feeds WHERE feed_id = 'nonexistent_feed_id_xyz';")
	if err != nil {
		t.Fatalf("empty query failed: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("expected 0 rows, got %d", len(rows))
	}
}

func TestDuckDBProcessMultipleQueries(t *testing.T) {
	duckdbBin := findTestDuckDB(t)
	dataPath := testDataPath(t)
	if dataPath == "" {
		t.Skip("data/ directory with Lance tables not found")
	}
	dbPath := filepath.Join(dataPath, "server.duckdb")

	cmd := exec.Command(duckdbBin, dbPath, "-c", "INSTALL lance FROM community;")
	cmd.CombinedOutput()

	proc, err := newDuckDBProcess(duckdbBin, dbPath, dataPath)
	if err != nil {
		t.Fatalf("newDuckDBProcess failed: %v", err)
	}
	defer proc.close()

	// Run 50 sequential queries
	for i := 0; i < 50; i++ {
		rows, err := proc.query("SELECT COUNT(*) AS cnt FROM _lance.main.feeds;")
		if err != nil {
			t.Fatalf("query %d failed: %v", i, err)
		}
		if len(rows) != 1 {
			t.Fatalf("query %d: expected 1 row, got %d", i, len(rows))
		}
	}
}

func TestDuckDBProcessRestart(t *testing.T) {
	duckdbBin := findTestDuckDB(t)
	dataPath := testDataPath(t)
	if dataPath == "" {
		t.Skip("data/ directory with Lance tables not found")
	}
	dbPath := filepath.Join(dataPath, "server.duckdb")

	cmd := exec.Command(duckdbBin, dbPath, "-c", "INSTALL lance FROM community;")
	cmd.CombinedOutput()

	proc, err := newDuckDBProcess(duckdbBin, dbPath, dataPath)
	if err != nil {
		t.Fatalf("newDuckDBProcess failed: %v", err)
	}
	defer proc.close()

	// Verify it works first
	rows, err := proc.query("SELECT COUNT(*) AS cnt FROM _lance.main.feeds;")
	if err != nil {
		t.Fatalf("pre-kill query failed: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("pre-kill: expected 1 row, got %d", len(rows))
	}
	originalPid := proc.pid()
	t.Logf("Original PID: %d", originalPid)

	// Kill the process externally
	if proc.cmd != nil && proc.cmd.Process != nil {
		proc.cmd.Process.Kill()
	}
	// Wait a moment for the kill to take effect
	time.Sleep(200 * time.Millisecond)

	// Next query should auto-restart and succeed
	rows, err = proc.query("SELECT COUNT(*) AS cnt FROM _lance.main.feeds;")
	if err != nil {
		t.Fatalf("post-kill query failed: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("post-kill: expected 1 row, got %d", len(rows))
	}

	newPid := proc.pid()
	t.Logf("New PID after restart: %d", newPid)
	if newPid == originalPid {
		t.Fatal("PID should have changed after restart")
	}
	if !proc.alive {
		t.Fatal("process should be alive after restart")
	}
}

func TestDuckDBProcessRestartRetry(t *testing.T) {
	duckdbBin := findTestDuckDB(t)
	dataPath := testDataPath(t)
	if dataPath == "" {
		t.Skip("data/ directory with Lance tables not found")
	}
	dbPath := filepath.Join(dataPath, "server.duckdb")

	cmd := exec.Command(duckdbBin, dbPath, "-c", "INSTALL lance FROM community;")
	cmd.CombinedOutput()

	proc, err := newDuckDBProcess(duckdbBin, dbPath, dataPath)
	if err != nil {
		t.Fatalf("newDuckDBProcess failed: %v", err)
	}
	defer proc.close()

	// Mark dead without killing (simulates detected death)
	proc.mu.Lock()
	proc.alive = false
	proc.kill()
	proc.mu.Unlock()

	// Query should detect dead process, restart, and return valid result
	rows, err := proc.query("SELECT COUNT(*) AS cnt FROM _lance.main.feeds;")
	if err != nil {
		t.Fatalf("restart-retry query failed: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	if !proc.alive {
		t.Fatal("process should be alive after restart-retry")
	}
}

func TestDuckDBProcessConcurrentQueries(t *testing.T) {
	duckdbBin := findTestDuckDB(t)
	dataPath := testDataPath(t)
	if dataPath == "" {
		t.Skip("data/ directory with Lance tables not found")
	}
	dbPath := filepath.Join(dataPath, "server.duckdb")

	cmd := exec.Command(duckdbBin, dbPath, "-c", "INSTALL lance FROM community;")
	cmd.CombinedOutput()

	proc, err := newDuckDBProcess(duckdbBin, dbPath, dataPath)
	if err != nil {
		t.Fatalf("newDuckDBProcess failed: %v", err)
	}
	defer proc.close()

	// 10 goroutines, 10 queries each
	var wg sync.WaitGroup
	errors := make(chan error, 100)

	for g := 0; g < 10; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				rows, err := proc.query("SELECT COUNT(*) AS cnt FROM _lance.main.feeds;")
				if err != nil {
					errors <- err
					return
				}
				if len(rows) != 1 {
					errors <- err
					return
				}
			}
		}(g)
	}

	wg.Wait()
	close(errors)

	var errs []error
	for e := range errors {
		errs = append(errs, e)
	}
	if len(errs) > 0 {
		t.Fatalf("%d errors in concurrent queries, first: %v", len(errs), errs[0])
	}
}

func TestDuckDBProcessClose(t *testing.T) {
	duckdbBin := findTestDuckDB(t)
	dataPath := testDataPath(t)
	if dataPath == "" {
		t.Skip("data/ directory with Lance tables not found")
	}
	dbPath := filepath.Join(dataPath, "server.duckdb")

	cmd := exec.Command(duckdbBin, dbPath, "-c", "INSTALL lance FROM community;")
	cmd.CombinedOutput()

	proc, err := newDuckDBProcess(duckdbBin, dbPath, dataPath)
	if err != nil {
		t.Fatalf("newDuckDBProcess failed: %v", err)
	}

	pid := proc.pid()
	if pid == 0 {
		t.Fatal("PID should be non-zero before close")
	}

	proc.close()

	if proc.alive {
		t.Fatal("process should not be alive after close")
	}

	// Verify the OS process is gone (give it a moment)
	time.Sleep(200 * time.Millisecond)
	p, err := os.FindProcess(pid)
	if err == nil && p != nil {
		// On Windows, FindProcess always succeeds - check if we can signal it
		// If the process is gone, Kill returns an error
		err := p.Kill()
		if err == nil {
			t.Log("Warning: process was still alive after close (killed it)")
		}
	}
}

func TestDuckDBProcessDoubleRestart(t *testing.T) {
	duckdbBin := findTestDuckDB(t)
	dataPath := testDataPath(t)
	if dataPath == "" {
		t.Skip("data/ directory with Lance tables not found")
	}
	dbPath := filepath.Join(dataPath, "server.duckdb")

	cmd := exec.Command(duckdbBin, dbPath, "-c", "INSTALL lance FROM community;")
	cmd.CombinedOutput()

	proc, err := newDuckDBProcess(duckdbBin, dbPath, dataPath)
	if err != nil {
		t.Fatalf("newDuckDBProcess failed: %v", err)
	}
	defer proc.close()

	// First kill + query
	if proc.cmd != nil && proc.cmd.Process != nil {
		proc.cmd.Process.Kill()
	}
	time.Sleep(200 * time.Millisecond)

	rows, err := proc.query("SELECT COUNT(*) AS cnt FROM _lance.main.feeds;")
	if err != nil {
		t.Fatalf("first restart query failed: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("first restart: expected 1 row, got %d", len(rows))
	}
	pid1 := proc.pid()

	// Second kill + query
	if proc.cmd != nil && proc.cmd.Process != nil {
		proc.cmd.Process.Kill()
	}
	time.Sleep(200 * time.Millisecond)

	rows, err = proc.query("SELECT COUNT(*) AS cnt FROM _lance.main.feeds;")
	if err != nil {
		t.Fatalf("second restart query failed: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("second restart: expected 1 row, got %d", len(rows))
	}
	pid2 := proc.pid()

	if pid1 == pid2 {
		t.Fatal("PID should change between restarts")
	}
	t.Logf("PIDs: original -> %d -> %d", pid1, pid2)
}
