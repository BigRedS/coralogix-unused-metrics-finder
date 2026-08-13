//go:build !race

package strintern

// raceEnabled is false in normal builds. Memory-retention measurements are only meaningful
// without the race detector, whose instrumentation changes both allocation and timing.
const raceEnabled = false
