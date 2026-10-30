//go:build race

package kernelboot_test

// raceDetectorEnabled is true in race-instrumented builds. Used to skip the
// genesis round-trip E2E on macOS, where the race detector's shadow memory
// collides with sqlite-vec's cgo mmap plus Argon2id's 64 MiB working set
// ("too many address space collisions"). Linux CI (47-bit address space) runs
// the test under race normally.
const raceDetectorEnabled = true
