//go:build lance_external

// lance_process.go - CUD operations via a persistent Python LanceDB sidecar.
//
// This is the build-tag alternative to lance_writer.go (native lancedb-go).
// Both define the same lanceWriter type with the same methods, so the rest
// of the codebase (lance_windows.go, lance_cgo.go, offline_cache.go) works
// unchanged.
//
// The Python process is started once and stays alive for the lifetime of the
// Go server.  Commands are sent as JSON lines on stdin; responses are read
// as JSON lines from stdout.  All calls are serialized through a mutex.
// If the process dies the next call auto-restarts it and retries once.
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
	"runtime"
	"strings"
	"sync"
	"time"

	"rss-lance/server/debug"
)

// lanceWriter wraps a persistent Python subprocess for Lance CUD operations.
// Same method set as the native lancedb-go lanceWriter in lance_writer.go.
type lanceWriter struct {
	dataPath string

	mu        sync.Mutex // serializes all commands
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	reader    *bufio.Reader
	alive     bool
	restartMu sync.Mutex
	procStart time.Time
	pythonBin string // resolved path to python interpreter

	// Version info captured from the first "info" call
	lancedbVersion string
	pyarrowVersion string

	logFn func(entry LogEntry) // optional log callback
}

// newLanceWriter spawns the Python sidecar and waits for the ready signal.
func newLanceWriter(dataPath string) (*lanceWriter, error) {
	pythonBin, err := findPython()
	if err != nil {
		return nil, fmt.Errorf("lance writer: %w", err)
	}

	w := &lanceWriter{
		dataPath:  dataPath,
		pythonBin: pythonBin,
	}
	if err := w.start(); err != nil {
		return nil, err
	}

	// Capture version info
	info, err := w.sendInfo()
	if err != nil {
		log.Printf("Warning: lance writer info command failed: %v", err)
	} else {
		w.lancedbVersion = info.LanceDBVersion
		w.pyarrowVersion = info.PyArrowVersion
		log.Printf("Lance Python writer started (pid %d), lancedb %s, pyarrow %s",
			info.PID, info.LanceDBVersion, info.PyArrowVersion)
	}

	return w, nil
}

// findPython locates the .venv Python interpreter.
func findPython() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("cannot determine executable path: %w", err)
	}
	base := filepath.Dir(exe)

	// Candidates relative to the server binary
	var candidates []string
	if runtime.GOOS == "windows" {
		candidates = []string{
			filepath.Join(base, ".venv", "Scripts", "python.exe"),
			filepath.Join(base, "..", ".venv", "Scripts", "python.exe"),
		}
	} else {
		candidates = []string{
			filepath.Join(base, ".venv", "bin", "python"),
			filepath.Join(base, "..", ".venv", "bin", "python"),
		}
	}

	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			abs, _ := filepath.Abs(c)
			return abs, nil
		}
	}

	// Fall back to PATH
	if p, err := exec.LookPath("python3"); err == nil {
		return p, nil
	}
	if p, err := exec.LookPath("python"); err == nil {
		return p, nil
	}

	return "", fmt.Errorf("python not found: checked %v and PATH", candidates)
}

// findLanceWriterScript locates tools/lance_writer.py relative to the binary.
func findLanceWriterScript() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("cannot determine executable path: %w", err)
	}
	base := filepath.Dir(exe)

	candidates := []string{
		filepath.Join(base, "tools", "lance_writer.py"),
		filepath.Join(base, "..", "tools", "lance_writer.py"),
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			abs, _ := filepath.Abs(c)
			return abs, nil
		}
	}
	return "", fmt.Errorf("tools/lance_writer.py not found: checked %v", candidates)
}

// start spawns the Python sidecar and waits for the {"ok":true,"ready":true} line.
func (w *lanceWriter) start() error {
	script, err := findLanceWriterScript()
	if err != nil {
		return err
	}

	cmd := exec.Command(w.pythonBin, script, w.dataPath)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("lance writer stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return fmt.Errorf("lance writer stdout: %w", err)
	}
	// Capture stderr for error logging
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("lance writer start: %w", err)
	}

	w.cmd = cmd
	w.stdin = stdin
	w.reader = bufio.NewReaderSize(stdout, 64*1024)
	w.alive = true
	w.procStart = time.Now()

	// Wait for the ready signal (with timeout)
	readyCh := make(chan error, 1)
	go func() {
		line, err := w.reader.ReadString('\n')
		if err != nil {
			readyCh <- fmt.Errorf("lance writer ready read: %w", err)
			return
		}
		var resp map[string]any
		if err := json.Unmarshal([]byte(line), &resp); err != nil {
			readyCh <- fmt.Errorf("lance writer ready parse: %w (line: %s)", err, strings.TrimSpace(line))
			return
		}
		if resp["ok"] != true || resp["ready"] != true {
			readyCh <- fmt.Errorf("lance writer unexpected ready response: %s", strings.TrimSpace(line))
			return
		}
		readyCh <- nil
	}()

	select {
	case err := <-readyCh:
		if err != nil {
			w.kill()
			return err
		}
	case <-time.After(30 * time.Second):
		w.kill()
		return fmt.Errorf("lance writer startup timed out (30s)")
	}

	debug.Log(debug.Lance, "lance python writer started (pid %d)", cmd.Process.Pid)
	return nil
}

// ── JSON protocol helpers ─────────────────────────────────────────────────────

// sendCmd sends a JSON command and reads the JSON response.
// Caller must NOT hold w.mu.
func (w *lanceWriter) sendCmd(cmd map[string]any) (map[string]any, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.sendCmdLocked(cmd)
}

// sendCmdLocked sends a command while the caller already holds w.mu.
func (w *lanceWriter) sendCmdLocked(cmd map[string]any) (map[string]any, error) {
	if !w.alive {
		if err := w.restartLocked(); err != nil {
			return nil, fmt.Errorf("lance writer dead and restart failed: %w", err)
		}
	}

	resp, err := w.writeAndRead(cmd)
	if err != nil {
		// Broken pipe or EOF -- try restart + retry once
		log.Printf("ERROR: Lance writer command failed (%v), restarting", err)
		w.emitLog("error", fmt.Sprintf("Lance writer command failed: %v, restarting", err))
		w.alive = false
		if restartErr := w.restartLocked(); restartErr != nil {
			return nil, fmt.Errorf("lance writer restart failed: %w", restartErr)
		}
		resp, err = w.writeAndRead(cmd)
		if err != nil {
			return nil, fmt.Errorf("lance writer failed after restart: %w", err)
		}
	}

	// Check for application-level errors
	if resp["ok"] != true {
		errMsg, _ := resp["error"].(string)
		return nil, fmt.Errorf("lance writer: %s", errMsg)
	}

	return resp, nil
}

// writeAndRead writes a JSON line to stdin and reads a JSON line from stdout.
func (w *lanceWriter) writeAndRead(cmd map[string]any) (map[string]any, error) {
	data, err := json.Marshal(cmd)
	if err != nil {
		return nil, fmt.Errorf("marshal cmd: %w", err)
	}

	debug.Log(debug.Lance, "LANCE CMD >>> %s", string(data))

	if _, err := fmt.Fprintf(w.stdin, "%s\n", data); err != nil {
		return nil, fmt.Errorf("write to lance writer: %w", err)
	}

	line, err := w.reader.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("read from lance writer: %w", err)
	}

	var resp map[string]any
	if err := json.Unmarshal([]byte(line), &resp); err != nil {
		return nil, fmt.Errorf("parse lance writer response: %w (line: %s)", err, strings.TrimSpace(line))
	}

	debug.Log(debug.Lance, "LANCE RESP <<< ok=%v", resp["ok"])
	return resp, nil
}

// ── Process lifecycle ─────────────────────────────────────────────────────────

func (w *lanceWriter) emitLog(level, message string) {
	if w.logFn == nil {
		return
	}
	now := time.Now().UTC()
	w.logFn(LogEntry{
		Timestamp: &now,
		Level:     level,
		Category:  "lifecycle",
		Message:   message,
	})
}

func (w *lanceWriter) restartLocked() error {
	w.restartMu.Lock()
	defer w.restartMu.Unlock()

	if w.alive {
		return nil // another goroutine already restarted
	}

	log.Printf("ERROR: Lance Python writer died -- restarting...")
	w.emitLog("error", "Lance Python writer died -- restarting")
	w.kill()

	if err := w.start(); err != nil {
		w.emitLog("error", fmt.Sprintf("Lance writer restart failed: %v", err))
		return fmt.Errorf("lance writer restart: %w", err)
	}

	log.Printf("Lance Python writer restarted successfully")
	w.emitLog("info", "Lance Python writer restarted successfully")
	return nil
}

func (w *lanceWriter) kill() {
	if w.stdin != nil {
		w.stdin.Close()
	}
	if w.cmd != nil && w.cmd.Process != nil {
		w.cmd.Process.Kill()
		w.cmd.Wait()
	}
	w.alive = false
}

func (w *lanceWriter) close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.kill()
	return nil
}

func (w *lanceWriter) pid() int {
	if w.cmd != nil && w.cmd.Process != nil {
		return w.cmd.Process.Pid
	}
	return 0
}

func (w *lanceWriter) uptime() time.Duration {
	if !w.alive || w.procStart.IsZero() {
		return 0
	}
	return time.Since(w.procStart)
}

// ProcessInfo returns version/status info for the server status API.
func (w *lanceWriter) ProcessInfo() *LanceProcessInfo {
	pid := w.pid()
	if pid == 0 {
		return nil
	}
	return &LanceProcessInfo{
		PID:            pid,
		UptimeSeconds:  int64(w.uptime().Seconds()),
		LanceDBVersion: w.lancedbVersion,
		PyArrowVersion: w.pyarrowVersion,
		Mode:           "external",
	}
}

// sendInfo calls the Python "info" command.
func (w *lanceWriter) sendInfo() (*LanceProcessInfo, error) {
	resp, err := w.sendCmd(map[string]any{"cmd": "info"})
	if err != nil {
		return nil, err
	}

	info, ok := resp["info"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("lance writer info: missing info field")
	}

	pid, _ := info["pid"].(float64)
	uptime, _ := info["uptime_seconds"].(float64)
	lv, _ := info["lancedb_version"].(string)
	pv, _ := info["pyarrow_version"].(string)

	return &LanceProcessInfo{
		PID:            int(pid),
		UptimeSeconds:  int64(uptime),
		LanceDBVersion: lv,
		PyArrowVersion: pv,
		Mode:           "external",
	}, nil
}

// ── Lance writer methods (same signatures as native lance_writer.go) ────────

// PutSetting updates the value column for an existing settings key.
func (w *lanceWriter) PutSetting(key, value string) error {
	_, err := w.sendCmd(map[string]any{
		"cmd":   "put_setting",
		"key":   key,
		"value": value,
	})
	return err
}

// PutSettingsBatch updates multiple settings in one call.
func (w *lanceWriter) PutSettingsBatch(settings map[string]string) error {
	_, err := w.sendCmd(map[string]any{
		"cmd":      "put_settings_batch",
		"settings": settings,
	})
	return err
}

// InsertSetting adds a new setting row.
func (w *lanceWriter) InsertSetting(key, value string) error {
	_, err := w.sendCmd(map[string]any{
		"cmd":   "insert_setting",
		"key":   key,
		"value": value,
	})
	return err
}

// InsertSettings adds multiple new setting rows in one batch.
func (w *lanceWriter) InsertSettings(settings map[string]string) error {
	if len(settings) == 0 {
		return nil
	}
	_, err := w.sendCmd(map[string]any{
		"cmd":      "insert_settings",
		"settings": settings,
	})
	return err
}

// UpdateArticle sets arbitrary columns on a single article.
func (w *lanceWriter) UpdateArticle(articleID string, updates map[string]interface{}) error {
	_, err := w.sendCmd(map[string]any{
		"cmd":        "update_article",
		"article_id": articleID,
		"updates":    updates,
	})
	return err
}

// SetArticleRead sets is_read on one article.
func (w *lanceWriter) SetArticleRead(articleID string, isRead bool) error {
	_, err := w.sendCmd(map[string]any{
		"cmd":        "set_article_read",
		"article_id": articleID,
		"is_read":    isRead,
	})
	return err
}

// SetArticleStarred sets is_starred on one article.
func (w *lanceWriter) SetArticleStarred(articleID string, isStarred bool) error {
	_, err := w.sendCmd(map[string]any{
		"cmd":        "set_article_starred",
		"article_id": articleID,
		"is_starred": isStarred,
	})
	return err
}

// MarkAllRead sets is_read=true for all unread articles in a feed.
func (w *lanceWriter) MarkAllRead(feedID string) error {
	_, err := w.sendCmd(map[string]any{
		"cmd":     "mark_all_read",
		"feed_id": feedID,
	})
	return err
}

// FlushOverrides merges a batch of pending article overrides into Lance.
func (w *lanceWriter) FlushOverrides(overrides map[string]*articleOverride) error {
	// Convert to the JSON format expected by the Python sidecar
	ovMap := make(map[string]map[string]any, len(overrides))
	for id, ov := range overrides {
		m := make(map[string]any)
		if ov.IsRead != nil {
			m["is_read"] = *ov.IsRead
		}
		if ov.IsStarred != nil {
			m["is_starred"] = *ov.IsStarred
		}
		if len(m) > 0 {
			ovMap[id] = m
		}
	}
	if len(ovMap) == 0 {
		return nil
	}
	_, err := w.sendCmd(map[string]any{
		"cmd":       "flush_overrides",
		"overrides": ovMap,
	})
	return err
}

// DeletePendingFeed removes a URL from the pending_feeds table.
func (w *lanceWriter) DeletePendingFeed(url string) error {
	_, err := w.sendCmd(map[string]any{
		"cmd": "delete_pending_feed",
		"url": url,
	})
	return err
}

// InsertPendingFeed adds a URL to the pending_feeds table.
func (w *lanceWriter) InsertPendingFeed(url, categoryID string) error {
	_, err := w.sendCmd(map[string]any{
		"cmd":         "insert_pending_feed",
		"url":         url,
		"category_id": categoryID,
	})
	return err
}

// DeleteOldLogs removes log entries matching the given filter expression.
func (w *lanceWriter) DeleteOldLogs(filter string) error {
	_, err := w.sendCmd(map[string]any{
		"cmd":    "delete_old_logs",
		"filter": filter,
	})
	return err
}

// logAPITableExists checks if the log_api Lance table directory exists on disk.
func (w *lanceWriter) logAPITableExists() bool {
	dir := filepath.Join(w.dataPath, "log_api.lance")
	info, err := os.Stat(dir)
	return err == nil && info.IsDir()
}

// InsertLogs writes a batch of log entries to the log_api table.
func (w *lanceWriter) InsertLogs(entries []LogEntry) error {
	if !w.logAPITableExists() {
		return nil
	}
	if len(entries) == 0 {
		return nil
	}

	// Convert LogEntry slice to JSON-serializable maps
	jEntries := make([]map[string]any, len(entries))
	for i, e := range entries {
		m := map[string]any{
			"log_id":   e.LogID,
			"level":    e.Level,
			"category": e.Category,
			"message":  e.Message,
			"details":  e.Details,
		}
		if e.Timestamp != nil {
			m["timestamp"] = e.Timestamp.UTC().Format(time.RFC3339Nano)
		}
		jEntries[i] = m
	}

	_, err := w.sendCmd(map[string]any{
		"cmd":     "insert_logs",
		"entries": jEntries,
	})
	return err
}

// getTableMeta returns lance-native metadata (version, columns, indexes) for a table.
func (w *lanceWriter) getTableMeta(name string) (version int, columns []ColumnInfo, indexes []LanceIndexInfo) {
	resp, err := w.sendCmd(map[string]any{
		"cmd":  "table_meta",
		"name": name,
	})
	if err != nil {
		return 0, nil, nil
	}

	if v, ok := resp["version"].(float64); ok {
		version = int(v)
	}

	if cols, ok := resp["columns"].([]any); ok {
		for _, c := range cols {
			if cm, ok := c.(map[string]any); ok {
				name, _ := cm["name"].(string)
				typ, _ := cm["type"].(string)
				columns = append(columns, ColumnInfo{Name: name, Type: typ})
			}
		}
	}

	// Python sidecar does not return indexes (lancedb Python API doesn't expose them easily)
	return
}
