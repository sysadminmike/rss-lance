package debug

import (
	"net/http"
	"strings"
	"time"
)

// responseCapture wraps http.ResponseWriter to capture the status code.
type responseCapture struct {
	http.ResponseWriter
	status int
	size   int
}

func (r *responseCapture) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *responseCapture) Write(b []byte) (int, error) {
	n, err := r.ResponseWriter.Write(b)
	r.size += n
	return n, err
}

// WrapHandler returns an http.Handler that logs request details when the
// "client" debug category is enabled.  Static file requests (CSS, JS,
// images, HTML) are excluded to reduce noise - only /api/ requests are
// logged.
func WrapHandler(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only log API requests
		if !strings.HasPrefix(r.URL.Path, "/api/") || !Enabled(Client) {
			h.ServeHTTP(w, r)
			return
		}

		start := time.Now()
		Log(Client, "--> %s %s %s", r.Method, r.URL.String(), r.RemoteAddr)

		rc := &responseCapture{ResponseWriter: w, status: 200}
		h.ServeHTTP(rc, r)

		dur := time.Since(start)
		Log(Client, "<-- %d %s %s (%s, %d bytes)",
			rc.status, r.Method, r.URL.Path, dur.Round(time.Microsecond), rc.size)
	})
}
