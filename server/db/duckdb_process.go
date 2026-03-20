//go:build windows || duckdb_cli

// Persistent DuckDB process for Windows and CLI-mode builds.
//
// Instead of spawning a new duckdb process per query (~600ms each), this keeps
// a single long-running process alive and pipes SQL via stdin/stdout.
// The Lance extension is loaded once at startup (LOAD lance) and the data
// directory attached once (ATTACH ... AS _lance).  Queries are delimited
// by a sentinel SELECT so the reader knows where one result ends and the
// next begins.
//
// DuckDB uses exclusive file locks on Windows, so only ONE process can
// ATTACH the data directory.  This is why we use a single process, not a
// pool.  All queries are serialized through a mutex.
//
// If the external process dies (crash, OOM, user kill), the next query
// detects it (broken pipe on write, or EOF on read) and automatically
// restarts the process, then retries the failed query once.
package db

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"rss-lance/server/debug"
)

// duckDBProcess wraps a persistent duckdb.exe subprocess.
// It provides query() and exec() methods that send SQL via stdin
// and read JSON results from stdout.
type duckDBProcess struct {
	duckdbBin string // path to duckdb.exe
	dbPath    string // local .duckdb file (for INSTALL cache)
	dataPath  string // lance data directory to ATTACH

	mu                sync.Mutex // serializes all queries
	cmd               *exec.Cmd
	stdin             io.WriteCloser
	reader            *bufio.Reader
	alive             bool
	restartMu         sync.Mutex // serializes restart attempts
	procStart         time.Time  // when the current duckdb.exe process was started
	stoppedForUpgrade bool       // when true, suppress auto-restart (user is replacing binary)

	// resultCh receives parsed JSON arrays from the reader goroutine.
	resultCh chan queryResult

	logFn func(entry LogEntry) // optional: structured log callback (Lance log table)

	// Version info captured on each start
	duckdbVersion string
	lanceVersion  string
}

type queryResult struct {
	rows []map[string]any
	err  error
}

// newDuckDBProcess creates and starts a persistent DuckDB process.
// Phase 1 (INSTALL) should already be done by the caller.
func newDuckDBProcess(duckdbBin, dbPath, dataPath string) (*duckDBProcess, error) {
	d := &duckDBProcess{
		duckdbBin: duckdbBin,
		dbPath:    dbPath,
		dataPath:  dataPath,
		resultCh:  make(chan queryResult, 4),
	}
	if err := d.start(); err != nil {
		return nil, err
	}
	return d, nil
}

// emitLog sends a structured log entry if a logFn callback is set.
func (d *duckDBProcess) emitLog(level, message string) {
	if d.logFn == nil {
		return
	}
	now := time.Now().UTC()
	d.logFn(LogEntry{
		Timestamp: &now,
		Level:     level,
		Category:  "lifecycle",
		Message:   message,
	})
}

// start spawns duckdb.exe :memory: -json with stdin/stdout pipes,
// loads the lance extension, attaches the data directory, and verifies
// the setup with a test query.
func (d *duckDBProcess) start() error {
	// Verify the data directory exists before spawning the process.
	// If the path was removed (e.g. renamed away), ATTACH would silently
	// create an empty Lance dataset, making queries return 0 rows instead
	// of failing — which prevents offline detection.
	if _, statErr := os.Stat(d.dataPath); statErr != nil {
		return fmt.Errorf("duckdb data path unavailable: %w", statErr)
	}

	cmd := exec.Command(d.duckdbBin, ":memory:", "-json")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("duckdb stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return fmt.Errorf("duckdb stdout pipe: %w", err)
	}
	// Discard stderr (DuckDB prints progress messages there)
	cmd.Stderr = nil

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("duckdb start: %w", err)
	}

	d.cmd = cmd
	d.stdin = stdin
	d.reader = bufio.NewReaderSize(stdout, 256*1024)
	d.alive = true

	// Drain any leftover results from previous channel
	for len(d.resultCh) > 0 {
		<-d.resultCh
	}

	// Start reader goroutine
	go d.readLoop()

	// Bootstrap: LOAD lance + ATTACH data directory
	lancePath := filepath.ToSlash(d.dataPath)
	bootSQL := fmt.Sprintf("LOAD lance;\nATTACH IF NOT EXISTS '%s' AS _lance (TYPE LANCE);\n", lancePath)

	// Send bootstrap SQL followed by a verification query
	_, err = fmt.Fprintf(d.stdin, "%sSELECT extension_name FROM duckdb_extensions() WHERE loaded=true AND extension_name='lance';\n", bootSQL)
	if err != nil {
		d.kill()
		return fmt.Errorf("duckdb bootstrap write: %w", err)
	}

	// Read the verification result (with timeout)
	result, err := d.readResultTimeout(15 * time.Second)
	if err != nil {
		d.kill()
		return fmt.Errorf("duckdb bootstrap read: %w", err)
	}

	// Check that lance extension is loaded
	if len(result) == 0 {
		d.kill()
		return fmt.Errorf("duckdb lance extension not loaded (verification query returned no rows)")
	}

	name, _ := result[0]["extension_name"].(string)
	if name != "lance" {
		d.kill()
		return fmt.Errorf("duckdb lance extension not loaded (got %q)", name)
	}

	// Query DuckDB version
	d.duckdbVersion = ""
	verRows, verErr := d.queryInternal("SELECT version() AS v")
	if verErr == nil && len(verRows) > 0 {
		if v, ok := verRows[0]["v"].(string); ok {
			d.duckdbVersion = v
		}
	}

	// Query Lance extension version
	d.lanceVersion = ""
	extRows, extErr := d.queryInternal("SELECT extension_version FROM duckdb_extensions() WHERE extension_name='lance' AND loaded=true")
	if extErr == nil && len(extRows) > 0 {
		if v, ok := extRows[0]["extension_version"].(string); ok {
			d.lanceVersion = v
		}
	}

	d.procStart = time.Now()
	log.Printf("DuckDB persistent process started (pid %d), duckdb %s, lance ext %s", cmd.Process.Pid, d.duckdbVersion, d.lanceVersion)
	d.emitLog("info", fmt.Sprintf("DuckDB persistent process started (pid %d), duckdb %s, lance ext %s", cmd.Process.Pid, d.duckdbVersion, d.lanceVersion))
	return nil
}

// readLoop runs in a goroutine, reading JSON arrays from DuckDB stdout.
// Each complete JSON array (tracked by bracket depth) is sent to resultCh.
func (d *duckDBProcess) readLoop() {
	var buf strings.Builder
	bracketDepth := 0
	inString := false
	escape := false

	for {
		line, err := d.reader.ReadString('\n')
		if len(line) > 0 {
			buf.WriteString(line)

			// Track bracket depth to know when a complete JSON array is received.
			// We track string literals to avoid counting brackets inside strings.
			for _, ch := range line {
				if escape {
					escape = false
					continue
				}
				if ch == '\\' && inString {
					escape = true
					continue
				}
				if ch == '"' {
					inString = !inString
					continue
				}
				if inString {
					continue
				}
				if ch == '[' {
					bracketDepth++
				} else if ch == ']' {
					bracketDepth--
				}
			}

			// A complete JSON array has been received when depth returns to 0
			if bracketDepth <= 0 && buf.Len() > 0 {
				raw := strings.TrimSpace(buf.String())
				buf.Reset()
				bracketDepth = 0
				inString = false
				escape = false

				if raw == "" {
					continue
				}

				rows, parseErr := parseJSONRows(raw)
				d.resultCh <- queryResult{rows: rows, err: parseErr}
			}
		}

		if err != nil {
			// EOF or pipe broken -- process died
			if buf.Len() > 0 {
				raw := strings.TrimSpace(buf.String())
				if raw != "" {
					rows, parseErr := parseJSONRows(raw)
					d.resultCh <- queryResult{rows: rows, err: parseErr}
				}
			}
			d.resultCh <- queryResult{err: fmt.Errorf("duckdb stdout: %w", err)}
			return
		}
	}
}

// parseJSONRows parses DuckDB JSON output into rows.
// Handles empty results and the Lance "[{]" quirk.
func parseJSONRows(raw string) ([]map[string]any, error) {
	if raw == "" || raw == "[]" {
		return nil, nil
	}
	// DuckDB Lance extension sometimes emits "[{]" for zero rows
	if raw == "[{]" {
		return nil, nil
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(raw), &rows); err != nil {
		return nil, fmt.Errorf("parse duckdb json: %w\nOutput: %s", err, truncate(raw, 500))
	}
	return rows, nil
}

// readResultTimeout reads one result from the reader goroutine with a timeout.

// isSentinelResult checks if the rows are actually the sentinel query's output.
// This happens when the real query errored (DuckDB sends errors to stderr,
// nothing to stdout), so the sentinel becomes the first result read.
func isSentinelResult(rows []map[string]any) bool {
	if len(rows) == 1 {
		if v, ok := rows[0]["_s"]; ok && v == "__SENTINEL__" {
			return true
		}
	}
	return false
}
func (d *duckDBProcess) readResultTimeout(timeout time.Duration) ([]map[string]any, error) {
	select {
	case r := <-d.resultCh:
		return r.rows, r.err
	case <-time.After(timeout):
		return nil, fmt.Errorf("duckdb query timed out after %v", timeout)
	}
}

// query sends SQL to the persistent process and returns parsed JSON rows.
// If the process is dead, it auto-restarts and retries once.
func (d *duckDBProcess) query(sql string) ([]map[string]any, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	debug.Log(debug.DuckDB, "PERSISTENT QUERY >>>\n%s", sql)

	if d.stoppedForUpgrade {
		return nil, fmt.Errorf("DuckDB is stopped for upgrade -- replace the binary and click Start")
	}

	if !d.alive {
		debug.Log(debug.DuckDB, "Process dead, restarting before query")
		if err := d.restart(false); err != nil {
			return nil, fmt.Errorf("duckdb process dead and restart failed: %w", err)
		}
	}

	rows, err := d.sendAndRead(sql)
	if err != nil {
		if d.stoppedForUpgrade {
			return nil, fmt.Errorf("DuckDB is stopped for upgrade -- replace the binary and click Start")
		}
		// Could be broken pipe / EOF -- try restart + retry once
		log.Printf("ERROR: DuckDB process query failed (%v), attempting restart", err)
		d.emitLog("error", fmt.Sprintf("DuckDB process query failed: %v, attempting restart", err))
		d.alive = false
		if restartErr := d.restart(false); restartErr != nil {
			log.Printf("ERROR: DuckDB process restart failed after query error: %v", restartErr)
			d.emitLog("error", fmt.Sprintf("DuckDB process restart failed after query error: %v", restartErr))
			return nil, fmt.Errorf("duckdb query failed and restart failed: query=%w, restart=%w", err, restartErr)
		}
		rows, err = d.sendAndRead(sql)
		if err != nil {
			return nil, fmt.Errorf("duckdb query failed after restart: %w", err)
		}
	}

	debug.Log(debug.DuckDB, "PERSISTENT QUERY OK: %d rows", len(rows))
	return rows, nil
}

// execStmt sends SQL that does not return meaningful rows (e.g. a write statement).
// Still reads the DuckDB response to keep the protocol in sync.
func (d *duckDBProcess) execStmt(sql string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	debug.Log(debug.DuckDB, "PERSISTENT EXEC >>>\n%s", sql)

	if d.stoppedForUpgrade {
		return fmt.Errorf("DuckDB is stopped for upgrade -- replace the binary and click Start")
	}

	if !d.alive {
		debug.Log(debug.DuckDB, "Process dead, restarting before exec")
		if err := d.restart(false); err != nil {
			return fmt.Errorf("duckdb process dead and restart failed: %w", err)
		}
	}

	_, err := d.sendAndRead(sql)
	if err != nil {
		if d.stoppedForUpgrade {
			return fmt.Errorf("DuckDB is stopped for upgrade -- replace the binary and click Start")
		}
		log.Printf("ERROR: DuckDB process exec failed (%v), attempting restart", err)
		d.emitLog("error", fmt.Sprintf("DuckDB process exec failed: %v, attempting restart", err))
		d.alive = false
		if restartErr := d.restart(false); restartErr != nil {
			log.Printf("ERROR: DuckDB process restart failed after exec error: %v", restartErr)
			d.emitLog("error", fmt.Sprintf("DuckDB process restart failed after exec error: %v", restartErr))
			return fmt.Errorf("duckdb exec failed and restart failed: exec=%w, restart=%w", err, restartErr)
		}
		_, err = d.sendAndRead(sql)
		if err != nil {
			return fmt.Errorf("duckdb exec failed after restart: %w", err)
		}
	}

	debug.Log(debug.DuckDB, "PERSISTENT EXEC OK")
	return nil
}

// sendAndRead writes SQL + sentinel to stdin and reads the real result
// (discarding the sentinel result).
func (d *duckDBProcess) sendAndRead(sql string) ([]map[string]any, error) {
	// Ensure SQL ends with semicolon — DuckDB interactive mode won't
	// execute a statement until it sees the terminating ';'.
	sql = strings.TrimSpace(sql)
	if !strings.HasSuffix(sql, ";") {
		sql += ";"
	}

	// Send the user's SQL followed by a sentinel query.
	// DuckDB will output: [real result JSON]\n[sentinel JSON]
	// We read two results: keep the first, discard the second.
	_, err := fmt.Fprintf(d.stdin, "%s\nSELECT '__SENTINEL__' AS _s;\n", sql)
	if err != nil {
		return nil, fmt.Errorf("write to duckdb: %w", err)
	}

	// Read real result
	realResult, err := d.readResultTimeout(30 * time.Second)
	if err != nil {
		return nil, err
	}

	// If the query errored, DuckDB sends nothing to stdout for it (error
	// goes to stderr).  The sentinel SELECT still succeeds, so it becomes
	// the first result we read.  Detect this and return empty rows instead
	// of waiting for a second sentinel that will never arrive.
	if isSentinelResult(realResult) {
		return nil, nil
	}

	// Read and discard sentinel result
	_, sentErr := d.readResultTimeout(10 * time.Second)
	if sentErr != nil {
		// Sentinel failed but we got the real result -- process may be dying
		// Mark as unhealthy so next query restarts
		d.alive = false
		log.Printf("ERROR: DuckDB sentinel read failed (%v), marking process unhealthy", sentErr)
		d.emitLog("error", fmt.Sprintf("DuckDB sentinel read failed: %v, marking process unhealthy", sentErr))
	}

	return realResult, nil
}

// restart kills the current process and starts a fresh one.
// graceful=true is used for intentional restarts (API-triggered); graceful=false
// is used when a crash is detected and the process needs to be recovered.
func (d *duckDBProcess) restart(graceful bool) error {
	d.restartMu.Lock()
	defer d.restartMu.Unlock()

	// If another goroutine already restarted while we waited for restartMu
	if d.alive {
		return nil
	}

	if graceful {
		log.Printf("Flushing DuckDB write cache and stopping process")
		d.emitLog("info", "Flushing DuckDB write cache and stopping process")
		d.kill()
		log.Printf("DuckDB process safely stopped")
		d.emitLog("info", "DuckDB process safely stopped")
	} else {
		log.Printf("ERROR: DuckDB persistent process died -- restarting...")
		d.emitLog("error", "DuckDB persistent process died -- restarting")
		d.kill()
	}

	if err := d.start(); err != nil {
		d.emitLog("error", fmt.Sprintf("DuckDB restart failed: %v", err))
		return fmt.Errorf("duckdb restart failed: %w", err)
	}

	log.Printf("DuckDB persistent process restarted successfully")
	d.emitLog("info", "DuckDB persistent process restarted successfully")
	return nil
}

// kill terminates the current duckdb.exe process.
func (d *duckDBProcess) kill() {
	if d.stdin != nil {
		d.stdin.Close()
	}
	if d.cmd != nil && d.cmd.Process != nil {
		d.cmd.Process.Kill()
		d.cmd.Wait()
	}
	d.alive = false
}

// close gracefully shuts down the persistent process.
func (d *duckDBProcess) close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.kill()
}

// queryInternal sends SQL via the sentinel protocol without acquiring the
// mutex.  It is intended for use during start() where the mutex is not
// held and no other goroutine has access yet.
func (d *duckDBProcess) queryInternal(sql string) ([]map[string]any, error) {
	return d.sendAndRead(sql)
}

// gracefulRestart acquires the query mutex (waiting for any running query
// to finish), then kills and restarts the DuckDB process.
func (d *duckDBProcess) gracefulRestart() error {
	d.mu.Lock()
	defer d.mu.Unlock()

	log.Printf("Graceful DuckDB restart requested")
	d.emitLog("info", "Graceful DuckDB restart requested")
	d.alive = false
	d.stoppedForUpgrade = false
	return d.restart(true)
}

// stopForUpgrade acquires the query mutex (waiting for any running query
// to finish), kills the DuckDB process, and sets the stoppedForUpgrade
// flag to suppress auto-restart.  The caller should flush the write cache
// before calling this.
func (d *duckDBProcess) stopForUpgrade() error {
	d.mu.Lock()
	defer d.mu.Unlock()

	log.Printf("DuckDB stop for upgrade requested")
	d.emitLog("info", "DuckDB stop for upgrade requested")
	d.kill()
	d.stoppedForUpgrade = true
	log.Printf("DuckDB stopped for upgrade (pid was %d)", d.pid())
	d.emitLog("info", "DuckDB stopped for upgrade -- safe to replace binary")
	return nil
}

// startAfterUpgrade clears the stoppedForUpgrade flag and starts a fresh
// DuckDB process.  Returns the new version info.
func (d *duckDBProcess) startAfterUpgrade() error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if !d.stoppedForUpgrade {
		return fmt.Errorf("DuckDB is not stopped for upgrade")
	}

	log.Printf("DuckDB start after upgrade requested")
	d.emitLog("info", "DuckDB start after upgrade requested")
	d.stoppedForUpgrade = false
	return d.restart(true)
}

// pid returns the PID of the running duckdb.exe process, or 0 if not running.
func (d *duckDBProcess) pid() int {
	if d.cmd != nil && d.cmd.Process != nil {
		return d.cmd.Process.Pid
	}
	return 0
}

// uptime returns the duration since the current duckdb.exe process was started.
func (d *duckDBProcess) uptime() time.Duration {
	if !d.alive || d.procStart.IsZero() {
		return 0
	}
	return time.Since(d.procStart)
}
