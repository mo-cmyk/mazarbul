//go:build !race

package kernelboot_test

// raceDetectorEnabled is false in non-instrumented builds. See the race-tagged
// sibling for why the genesis round-trip E2E skips under the macOS race
// detector.
const raceDetectorEnabled = false
