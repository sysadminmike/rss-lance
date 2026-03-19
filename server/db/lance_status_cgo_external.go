//go:build !windows && !duckdb_cli && lance_external

package db

// LanceWriterInfo returns process info from the Python sidecar.
func (s *cgoStore) LanceWriterInfo() *LanceProcessInfo {
	if s.writer == nil {
		return nil
	}
	return s.writer.ProcessInfo()
}
