package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	rtdebug "runtime/debug"
	"strings"
	"time"

	"rss-lance/server/api"
	"rss-lance/server/db"
	"rss-lance/server/debug"

	"github.com/BurntSushi/toml"
)

// Build info variables -- injected at compile time via -ldflags.
var (
	BuildTime           string // e.g. "2026-03-18T14:30:00Z"
	BuildVersion        string // e.g. "v1.0.0" or "test-abc123"
	BuildDuckDBVersion  string // e.g. "v1.5.0" -- DuckDB CLI version at build time
	BuildLanceExtVersion string // e.g. "0.0.4" -- Lance extension version at build time
)

// Config 
type Config struct {
	Storage struct {
		Type       string `toml:"type"`
		Path       string `toml:"path"`
		DuckDBPath string `toml:"duckdb_path"`
	} `toml:"storage"`
	Server struct {
		Host          string `toml:"host"`
		Port          int    `toml:"port"`
		FrontendDir   string `toml:"frontend_dir"`
		ShowShutdown  bool   `toml:"show_shutdown"`
	} `toml:"server"`
}

// isCloudURI returns true for cloud storage URIs (s3://, gs://, az://).
func isCloudURI(path string) bool {
	return strings.HasPrefix(path, "s3://") ||
		strings.HasPrefix(path, "gs://") ||
		strings.HasPrefix(path, "az://")
}

func loadConfig(path string) (*Config, error) {
	cfg := &Config{}
	cfg.Server.Host = "127.0.0.1"
	cfg.Server.Port = 8080
	cfg.Server.FrontendDir = "./frontend"
	cfg.Storage.Type = "local"
	cfg.Storage.Path = "./data"

	if _, err := os.Stat(path); err == nil {
		if _, err := toml.DecodeFile(path, cfg); err != nil {
			return nil, fmt.Errorf("parse config: %w", err)
		}
	}
	return cfg, nil
}

func main() {
	configPath := flag.String("config", "config.toml", "path to config.toml")
	debugFlag := flag.String("debug", "", "comma-separated debug categories: client,duckdb,batch,lance,all")
	portFlag := flag.Int("port", 0, "override server port from config")
	flag.Parse()

	// Enable debug from --debug flag or RSS_LANCE_DEBUG env var
	dbgStr := *debugFlag
	if dbgStr == "" {
		dbgStr = os.Getenv("RSS_LANCE_DEBUG")
	}
	debug.Parse(dbgStr)

	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatalf("config error: %v", err)
	}

	// CLI --port overrides config file
	if *portFlag > 0 {
		cfg.Server.Port = *portFlag
	}

	// Resolve data path relative to config file location
	// Cloud URIs (s3://, gs://, az://) are passed through as-is
	dataPath := cfg.Storage.Path
	if !isCloudURI(dataPath) && !filepath.IsAbs(dataPath) {
		dir := filepath.Dir(*configPath)
		dataPath = filepath.Join(dir, dataPath)
	}

	// Resolve frontend path
	frontendDir := cfg.Server.FrontendDir
	if !filepath.IsAbs(frontendDir) {
		dir := filepath.Dir(*configPath)
		frontendDir = filepath.Join(dir, frontendDir)
	}

	// Resolve duckdb_path if set (must be local storage for reliable locking)
	duckdbPath := cfg.Storage.DuckDBPath
	if duckdbPath != "" && !filepath.IsAbs(duckdbPath) {
		dir := filepath.Dir(*configPath)
		duckdbPath = filepath.Join(dir, duckdbPath)
	}

	// Open DB
	store, err := db.Open(dataPath, duckdbPath)
	if err != nil {
		log.Fatalf("db open: %v", err)
	}
	defer store.Close()

	// Build router
	mux := http.NewServeMux()

	// Server start time for uptime calculation
	serverStartTime := time.Now()

	// REST API
	feedsHandler := api.NewFeedsHandler(store)
	articlesHandler := api.NewArticlesHandler(store)
	categoriesHandler := api.NewCategoriesHandler(store)
	settingsHandler := api.NewSettingsHandler(store)
	statusHandler := api.NewStatusHandler(store)
	logsHandler := api.NewLogsHandler(store)
	tablesHandler := api.NewTablesHandler(store)
	offlineStatusHandler := api.NewOfflineStatusHandler(store)
	serverStatusHandler := api.NewServerStatusHandler(serverStartTime, BuildTime, BuildVersion, BuildDuckDBVersion, BuildLanceExtVersion, func() api.CacheStatsInfo {
		reads, stars := store.CacheStats()
		return api.CacheStatsInfo{
			PendingReads: reads,
			PendingStars: stars,
		}
	}, func() *api.DuckDBProcessInfo {
		info := store.DuckDBProcessInfo()
		if info == nil {
			return nil
		}
		return &api.DuckDBProcessInfo{
			PID:           info.PID,
			UptimeSeconds: info.UptimeSeconds,
			DuckDBVersion: info.DuckDBVersion,
			LanceVersion:  info.LanceVersion,
			Stopped:       info.Stopped,
		}
	}, func() *api.LogBufferStatsInfo {
		st := store.LogBufferStats()
		return &api.LogBufferStatsInfo{
			MemoryEntries: st.MemoryEntries,
			DuckDBEntries: st.DuckDBEntries,
			InfraEvents:   st.InfraEvents,
		}
	})

	// Stats collector: samples runtime metrics every 5s, keeps configurable window in memory
	statsCollector := api.NewStatsCollector(store.GetSetting)
	statsCollector.Start()
	defer statsCollector.Stop()

	// Periodic log trimming: Go server trims its own log_api table every 5 minutes
	logTrimCtx, logTrimCancel := context.WithCancel(context.Background())
	defer logTrimCancel()
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				mode := "count"
				if v, found, err := store.GetSetting("log.retention_mode"); err == nil && found {
					// Strip JSON quotes if present
					m := strings.Trim(v, `"`)
					if m == "age" {
						mode = "age"
					}
				}
				if mode == "age" {
					maxAge := 30
					if v, found, err := store.GetSetting("log.max_age_days"); err == nil && found {
						if n, e := fmt.Sscanf(strings.Trim(v, `"`), "%d", &maxAge); n == 1 && e == nil && maxAge > 0 {
							// use parsed value
						} else {
							maxAge = 30
						}
					}
					if err := store.TrimLogsByAge(maxAge); err != nil {
						log.Printf("log trim (age) error: %v", err)
					}
				} else {
					maxEntries := 10000
					if v, found, err := store.GetSetting("log.max_entries"); err == nil && found {
						if n, e := fmt.Sscanf(strings.Trim(v, `"`), "%d", &maxEntries); n == 1 && e == nil {
							// use parsed value
						} else {
							maxEntries = 10000
						}
					}
					if err := store.TrimLogs(maxEntries); err != nil {
						log.Printf("log trim error: %v", err)
					}
				}
			case <-logTrimCtx.Done():
				return
			}
		}
	}()

	// Server logger for structured log events
	serverLogger := api.NewServerLogger(store)
	settingsHandler.SetLogger(serverLogger)
	feedsHandler.SetLogger(serverLogger)
	articlesHandler.SetLogger(serverLogger)

	mux.HandleFunc("/api/feeds", feedsHandler.Handle)
	mux.HandleFunc("/api/feeds/", feedsHandler.Handle)
	mux.HandleFunc("/api/articles/", articlesHandler.Handle)
	mux.HandleFunc("/api/categories", categoriesHandler.Handle)
	mux.HandleFunc("/api/categories/", categoriesHandler.Handle)
	mux.HandleFunc("/api/settings", settingsHandler.Handle)
	mux.HandleFunc("/api/settings/", settingsHandler.Handle)
	mux.HandleFunc("/api/status", statusHandler.Handle)
	mux.HandleFunc("/api/server-status", serverStatusHandler.Handle)
	mux.HandleFunc("/api/server-status/history", statsCollector.Handle)
	mux.HandleFunc("/api/logs", logsHandler.Handle)
	mux.HandleFunc("/api/tables/", tablesHandler.Handle)
	mux.HandleFunc("/api/offline-status", offlineStatusHandler.Handle)

	// POST /api/flush -- trigger immediate write-cache flush to Lance
	mux.HandleFunc("/api/flush", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		store.FlushPendingChanges()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})

	// POST /api/duckdb/restart -- graceful DuckDB process restart
	mux.HandleFunc("/api/duckdb/restart", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		serverLogger.Log("info", "lifecycle", "DuckDB graceful restart requested via API", "")
		if err := store.RestartDuckDB(); err != nil {
			serverLogger.Log("error", "lifecycle", "DuckDB graceful restart failed: "+err.Error(), "")
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		serverLogger.Log("info", "lifecycle", "DuckDB graceful restart completed", "")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})

	// POST /api/duckdb/stop -- flush cache and stop DuckDB for binary upgrade
	mux.HandleFunc("/api/duckdb/stop", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		serverLogger.Log("info", "lifecycle", "DuckDB stop for upgrade requested via API", "")
		if err := store.StopDuckDB(); err != nil {
			serverLogger.Log("error", "lifecycle", "DuckDB stop for upgrade failed: "+err.Error(), "")
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		serverLogger.Log("info", "lifecycle", "DuckDB stopped for upgrade -- safe to replace binary", "")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "message": "DuckDB stopped. Safe to replace binary."})
	})

	// POST /api/duckdb/start -- start DuckDB after binary upgrade
	mux.HandleFunc("/api/duckdb/start", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		serverLogger.Log("info", "lifecycle", "DuckDB start after upgrade requested via API", "")
		if err := store.StartDuckDB(); err != nil {
			serverLogger.Log("error", "lifecycle", "DuckDB start after upgrade failed: "+err.Error(), "")
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		serverLogger.Log("info", "lifecycle", "DuckDB started after upgrade", "")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})

	// POST /api/duck-hunt -- easter egg: log duck hunt results
	mux.HandleFunc("/api/duck-hunt", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			Hit bool `json:"hit"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		if body.Hit {
			hitMsgs := []string{
				"BOOM! The DuckDB duck has been obliterated! SELECT * FROM ducks WHERE alive = true returns 0 rows.",
				"Critical hit! DuckDB took 9999 damage. It's super effective!",
				"The duck has been DROP TABLE'd! No ROLLBACK can save it now.",
				"Duck terminated. Process exited with code QUACK. RIP in peace, little database bird.",
				"Achievement unlocked: Duck Destroyer! Your data is now powered by pure chaos.",
				"The DuckDB duck has been sent to /dev/null. It will be missed by approximately 0 people.",
				"FATALITY! DuckDB has been defragmented... permanently.",
			}
			msg := hitMsgs[time.Now().UnixNano()%int64(len(hitMsgs))]
			serverLogger.LogJSON("info", "easter_eggs", msg,
				map[string]any{"game": "duck-hunt", "result": "hit", "quack_factor": 9001})
		} else {
			missMsgs := []string{
				"MISS! The duck escapes! It quacks mockingly as it flies into the sunset. Better luck next time!",
				"Swing and a miss! The DuckDB duck lives to OLAP another day.",
				"You missed! The duck does a victory lap. Your aim is worse than a full table scan.",
				"The duck dodged! It whispers 'my indexes are faster than your reflexes' as it flies away.",
				"MISS! The duck survives. It will remember this. Expect slower queries as revenge.",
				"Not even close! The duck quacks 'Have you tried EXPLAIN ANALYZE on your aim?'",
				"You missed! DuckDB remains undefeated. Your shot went straight to the WAL... of shame.",
				"MISS! That aim was totally out of ORDER. DuckDB just sorted you out.",
				"Missed! You tried to hit the duck, but it was already compressed into a tiny target. Better luck next SCAN!",
				"Swing and a miss! You just fired into an empty partition. The duck is in another block!",
				"MISS! Your shot was too slow for an in-memory duck. It migrated before you even pulled the TRIGGER.",
				"MISS! You were close, but not within the similarity threshold. LanceDB stays in the neighborhood.",
				"Swing and a miss! Your aim has zero dimensionality. The vector remains un-indexed.",
				"You missed! That shot wasn't even in the top K results. LanceDB is still at the top of the pile.",
				"Missed! Your trajectory was way out of bounds. Even LanceDB can't find where that shot landed.",
				"Shot failed! You tried to hit it, but LanceDB just versioned itself away. You're shooting at a ghost.",
				"Swing and a miss! You hit the metadata, but the duck is stored in a different fragment.",
				"LanceDB finds nearest neighbors in under 1ms, but it's been 10 seconds and you still haven't found the duck.",
				"MISS! You just performed a DELETE without a WHERE clause. Your accuracy is now zero.",
				"You missed! The duck just JOINED another flock. Good luck finding the right key now.",
				"MISS! Your aim is NULL. It doesn't even exist in this reality.",
				"You missed! Error 404: Accuracy not found. Please REINDEX your eyeballs.",
				"Swing and a miss! You just tried to scan the whole table when a random-access shot was required.",
				"Your aim has higher latency than a cross-region JOIN. Pull the trigger already!",
				"Error: Connection Timeout. Even the duck got bored waiting for your execution plan.",
				"MISS! Your reaction time is so slow, you're basically a SELECT * on a billion rows without an index.",
			}
			msg := missMsgs[time.Now().UnixNano()%int64(len(missMsgs))]
			serverLogger.LogJSON("info", "easter_eggs", msg,
				map[string]any{"game": "duck-hunt", "result": "miss", "quack_factor": 0})
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})

	// GET /api/config — expose selected config values to the frontend
	mux.HandleFunc("/api/config", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"show_shutdown":  cfg.Server.ShowShutdown,
		})
	})

	// Static frontend
	fs := http.FileServer(http.Dir(frontendDir))

	// Serve custom.css from the settings DB (key "custom_css").
	// Falls back to data/custom.css file for backward compatibility.
	// Returns empty CSS if neither exists so the browser never gets a 404.
	customCSSPath := filepath.Join(dataPath, "custom.css")
	mux.HandleFunc("/css/custom.css", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		if val, found, err := store.GetSetting("custom_css"); err == nil && found {
			var css string
			if json.Unmarshal([]byte(val), &css) == nil && css != "" {
				w.Write([]byte(css))
				return
			}
		}
		// Fallback to file on disk
		if data, err := os.ReadFile(customCSSPath); err == nil {
			w.Write(data)
		}
	})

	mux.Handle("/", fs)

	// Extract VCS revision once at startup for the build-revision header
	vcsRevision := ""
	if bi, ok := rtdebug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			if s.Key == "vcs.revision" {
				vcsRevision = s.Value
				break
			}
		}
	}

	// Wrap with debug middleware (logs API requests when "client" debug is on)
	handler := debug.WrapHandler(api.BuildRevisionMiddleware(api.RequestLoggerMiddleware(mux, serverLogger), vcsRevision))

	addr := fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port)

	srv := &http.Server{Addr: addr, Handler: handler}

	// POST /api/shutdown — graceful server stop (only if show_shutdown is true)
	if cfg.Server.ShowShutdown {
		mux.HandleFunc("/api/shutdown", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]bool{"ok": true})
			log.Println("Shutdown requested via API")
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				srv.Shutdown(ctx)
			}()
		})
	}

	log.Printf("Data directory: %s", dataPath)
	log.Printf("Frontend:       %s", frontendDir)
	log.Printf("Debug:          %s", debug.Summary())
	if BuildVersion != "" {
		log.Printf("Build:          %s", BuildVersion)
	}
	if BuildTime != "" {
		log.Printf("Built at:       %s", BuildTime)
	}
	fmt.Println()
	fmt.Println("  RSS-Lance is ready!")
	fmt.Printf("  Open in browser --> http://%s\n", addr)
	fmt.Println()

	serverLogger.Log("info", "lifecycle", "Server started on "+addr, "")

	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatalf("server error: %v", err)
	}
	serverLogger.Log("info", "lifecycle", "Server stopped", "")
	log.Println("Server stopped.")
}
