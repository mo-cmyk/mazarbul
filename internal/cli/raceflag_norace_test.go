//go:build !race

package cli

// raceDetectorEnabled is false in non-instrumented builds. See the race-tagged
// sibling for why the mellon pairing round-trips skip under the macOS race
// detector.
const raceDetectorEnabled = false
