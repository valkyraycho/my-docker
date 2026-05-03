//go:build linux

package state

// WithTempDir temporarily points the package's container state
// directory at dir. Returns a function that restores the original;
// callers must defer it.
//
// Intended for tests. Not safe for concurrent use: the package var
// is process-wide, so two parallel tests using WithTempDir would
// race. Tests using it must not t.Parallel.
func WithTempDir(dir string) func() {
	old := containersDir
	containersDir = dir
	return func() { containersDir = old }
}
