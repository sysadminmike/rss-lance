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
	"strings"
	"time"

	"rss-lance/server/api"
	"rss-lance/server/db"
	"rss-lance/server/debug"

	"github.com/BurntSushi/toml"
)

// Config mirrors config.toml
type Config struct {
	Storage struct {
		Type string `toml:"type"`
		Path string `toml:"path"`
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

	// Open DB
	store, err := db.Open(dataPath)
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
	serverStatusHandler := api.NewServerStatusHandler(serverStartTime, func() api.CacheStatsInfo {
		reads, stars := store.CacheStats()
		return api.CacheStatsInfo{
			PendingReads: reads,
			PendingStars: stars,
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

	// Wrap with debug middleware (logs API requests when "client" debug is on)
	handler := debug.WrapHandler(mux)

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
