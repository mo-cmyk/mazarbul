//go:build !race

package doctor

// raceDetectorEnabled is false in non-instrumented builds. See the race-tagged
// sibling for why the self-principal repair E2E skips under the macOS race
// detector.
const raceDetectorEnabled = false
