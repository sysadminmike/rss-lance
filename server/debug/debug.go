// Package debug provides a categorised debug-logging system for RSS-Lance.
//
// Categories:
//
//	client  – HTTP request/response details (method, path, status, duration)
//	duckdb  – every SQL statement sent to DuckDB (queries and execs)
//	batch   – write-cache lifecycle (set, flush, counts)
//	lance   – Lance file operations (paths resolved, bootstrap)
//	all     – enables every category above
//
// Enable via CLI flag:  --debug client,duckdb     (comma-separated)
// Or set the env var:   RSS_LANCE_DEBUG=all
package debug

import (
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
)

// Category is a debug log category.
type Category string

const (
	Client Category = "client" // HTTP requests & responses
	DuckDB Category = "duckdb" // SQL sent to DuckDB
	Batch  Category = "batch"  // write-cache operations
	Lance  Category = "lance"  // Lance file / path operations
	All    Category = "all"    // meta-category: enables everything
)

// AllCategories lists concrete categories (not "all").
var AllCategories = []Category{Client, DuckDB, Batch, Lance}

var (
	mu      sync.RWMutex
	enabled = map[Category]bool{}
	logger  = log.New(os.Stderr, "[DEBUG] ", log.Ldate|log.Ltime|log.Lmicroseconds)
)

// Enable turns on one or more categories.  "all" enables every category.
func Enable(cats ...Category) {
	mu.Lock()
	defer mu.Unlock()
	for _, c := range cats {
		if c == All {
			for _, ac := range AllCategories {
				enabled[ac] = true
			}
		} else {
			enabled[c] = true
		}
	}
}

// Enabled reports whether a category is active.
func Enabled(cat Category) bool {
	mu.RLock()
	defer mu.RUnlock()
	return enabled[cat]
}

// Log prints a debug message if the given category is enabled.
func Log(cat Category, format string, args ...any) {
	mu.RLock()
	on := enabled[cat]
	mu.RUnlock()
	if !on {
		return
	}
	msg := fmt.Sprintf(format, args...)
	logger.Printf("[%s] %s", cat, msg)
}

// Parse parses a comma-separated debug string (e.g. "client,duckdb")
// and enables the matching categories.  Unknown tokens are reported to stderr.
func Parse(s string) {
	if s == "" {
		return
	}
	for _, tok := range strings.Split(s, ",") {
		tok = strings.TrimSpace(strings.ToLower(tok))
		if tok == "" {
			continue
		}
		cat := Category(tok)
		switch cat {
		case Client, DuckDB, Batch, Lance, All:
			Enable(cat)
		default:
			fmt.Fprintf(os.Stderr, "warning: unknown debug category %q (valid: client, duckdb, batch, lance, all)\n", tok)
		}
	}

	// Print enabled categories for confirmation
	mu.RLock()
	var cats []string
	for _, c := range AllCategories {
		if enabled[c] {
			cats = append(cats, string(c))
		}
	}
	mu.RUnlock()
	if len(cats) > 0 {
		log.Printf("Debug logging enabled: %s", strings.Join(cats, ", "))
	}
}

// Summary returns a human-readable summary of what is enabled.
func Summary() string {
	mu.RLock()
	defer mu.RUnlock()
	var cats []string
	for _, c := range AllCategories {
		if enabled[c] {
			cats = append(cats, string(c))
		}
	}
	if len(cats) == 0 {
		return "none"
	}
	return strings.Join(cats, ", ")
}
