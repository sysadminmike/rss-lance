//go:build (windows || duckdb_cli) && lance_external

package db

// LanceWriterInfo returns process info from the Python sidecar.
func (s *cliStore) LanceWriterInfo() *LanceProcessInfo {
	if s.writer == nil {
		return nil
	}
	return s.writer.ProcessInfo()
}
