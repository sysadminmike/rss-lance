//go:build !cgo

package db

import (
	"database/sql"
	"fmt"
)

// openOfflineDuckDB returns an error when CGo is not available.
// The offline cache requires the go-duckdb driver which needs CGo.
// Callers handle this gracefully: offCache stays nil and the server
// continues without offline caching or log fallback to DuckDB.
func openOfflineDuckDB(path string) (*sql.DB, error) {
	return nil, fmt.Errorf("offline cache unavailable: go-duckdb requires CGo (build with CGO_ENABLED=1 and GCC)")
}
