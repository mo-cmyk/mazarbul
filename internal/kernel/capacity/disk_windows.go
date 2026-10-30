//go:build windows

package capacity

// freeDisk on Windows is not implemented. Returns 0 so the panel
// renders a "—" placeholder; a syscall-backed implementation can land
// later if Windows usage warrants it.
func freeDisk(_ string) int64 { return 0 }
