//go:build !darwin

package doctor

// tmExclusionStatus is a no-op outside Darwin: the Time Machine
// xattr is meaningless on other OSes. Returns OK so the unified
// doctor row reports green without distinguishing platforms.
func tmExclusionStatus(_ string) (string, bool) { return "", true }
