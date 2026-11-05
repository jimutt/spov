//go:build !unix

package credproxy

// lockFile is a no-op where the sidecar never runs.
func lockFile(string) (func(), error) { return func() {}, nil }
