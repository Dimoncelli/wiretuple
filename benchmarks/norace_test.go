//go:build !race

package benchmarks

// raceEnabled reports whether the race detector is active.
func raceEnabled() bool { return false }
