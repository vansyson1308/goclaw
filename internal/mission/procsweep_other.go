//go:build !linux

package mission

// killProcessesUnder is only implemented on Linux.
func killProcessesUnder(string) []int { return nil }
