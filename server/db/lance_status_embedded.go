//go:build (windows || duckdb_cli) && !lance_external

package db

// LanceWriterInfo returns embedded-mode info when using the native lancedb-go writer.
func (s *cliStore) LanceWriterInfo() *LanceProcessInfo {
	return &LanceProcessInfo{Mode: "embedded"}
}
