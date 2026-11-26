//go:build race

package inbox

// raceEnabled reports whether the race detector is on; it slows pure-Go
// SQLite by an order of magnitude, so latency budgets are asserted only
// without it.
const raceEnabled = true
