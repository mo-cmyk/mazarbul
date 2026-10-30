//go:build !darwin

package daemon

// applyTimeMachineExclusion is a no-op outside Darwin. The Spotlight
// marker file is still written cross-platform so ApplyDataDirHygiene
// remains testable on Linux CI runners.
func applyTimeMachineExclusion(_ string) error { return nil }
