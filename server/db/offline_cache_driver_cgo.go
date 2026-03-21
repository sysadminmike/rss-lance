//go:build cgo

package db

import (
	"database/sql"

	_ "github.com/marcboeker/go-duckdb/v2"
)

// openOfflineDuckDB opens the offline cache DuckDB file via the embedded
// go-duckdb driver (requires CGo / GCC at build time).
func openOfflineDuckDB(path string) (*sql.DB, error) {
	return sql.Open("duckdb", path)
}
