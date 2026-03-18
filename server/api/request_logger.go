package api

import (
	"fmt"
	"net/http"
	"strings"
	"time"
)

// BuildRevisionMiddleware injects an X-Build-Revision response header on
// every /api/ request so the frontend can detect server restarts / upgrades.
func BuildRevisionMiddleware(next http.Handler, revision string) http.Handler {
	if revision == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("X-Build-Revision", revision)
		}
		next.ServeHTTP(w, r)
	})
}

// statusCapture wraps http.ResponseWriter to capture the response status code.
type statusCapture struct {
	http.ResponseWriter
	code int
}

func (sc *statusCapture) WriteHeader(code int) {
	sc.code = code
	sc.ResponseWriter.WriteHeader(code)
}

// RequestLoggerMiddleware returns an http.Handler that logs API requests via
// the structured logging system (log.api.requests and log.api.errors).
func RequestLoggerMiddleware(next http.Handler, logger *ServerLogger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only log /api/ requests, skip static files
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}

		start := time.Now()
		sc := &statusCapture{ResponseWriter: w, code: 200}
		next.ServeHTTP(sc, r)
		dur := time.Since(start)

		// log.api.requests -- all API requests
		logger.LogJSON("info", "requests",
			fmt.Sprintf("%s %s %d", r.Method, r.URL.Path, sc.code),
			map[string]any{
				"method":   r.Method,
				"path":     r.URL.Path,
				"status":   sc.code,
				"duration": dur.String(),
			})

		// log.api.errors -- 4xx/5xx responses
		if sc.code >= 400 {
			logger.LogJSON("warn", "errors",
				fmt.Sprintf("%s %s returned %d", r.Method, r.URL.Path, sc.code),
				map[string]any{
					"method": r.Method,
					"path":   r.URL.Path,
					"status": sc.code,
				})
		}
	})
}
