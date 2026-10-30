//go:build race

package cli

// raceDetectorEnabled is true in race-instrumented builds. Used to skip the
// mellon pairing round-trips on macOS, where the race detector's shadow memory
// collides with Argon2id's 64 MiB working set ("too many address space
// collisions"). Linux CI (47-bit address space) runs them under race normally.
// Mirrors internal/kernelboot's identical guard for its Argon2id genesis E2E.
const raceDetectorEnabled = true
