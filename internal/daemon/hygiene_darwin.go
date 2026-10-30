package daemon

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// timeMachineExcludeXattr is the extended attribute Time Machine
// reads to decide whether to skip a path during backup. The same
// flag `tmutil addexclusion` writes; backupd parses the bplist
// value (see timeMachineExcludeBplist) rather than treating mere
// presence as the signal.
const timeMachineExcludeXattr = "com.apple.metadata:com_apple_backup_excludeItem"

// timeMachineExcludeBplist is the exact 61-byte binary plist
// `tmutil addexclusion` writes for the
// `com.apple.metadata:com_apple_backup_excludeItem` xattr. It
// encodes a single ASCII string "com.apple.backupd" (the bundle id
// of the backup daemon). MacOS Time Machine reads + parses the
// plist value, not just the xattr presence: writing the byte
// sequence verbatim is the only encoding the OS honours across
// Sequoia + Tahoe (verified against `tmutil addexclusion` on
// macOS 26.3.1).
//
//nolint:gochecknoglobals // package-level constant byte slice; immutable, no init order risk
var timeMachineExcludeBplist = []byte{
	0x62, 0x70, 0x6C, 0x69, 0x73, 0x74, 0x30, 0x30,
	0x5F, 0x10, 0x11, 0x63, 0x6F, 0x6D, 0x2E, 0x61,
	0x70, 0x70, 0x6C, 0x65, 0x2E, 0x62, 0x61, 0x63,
	0x6B, 0x75, 0x70, 0x64, 0x08, 0x00, 0x00, 0x00,
	0x00, 0x00, 0x00, 0x01, 0x01, 0x00, 0x00, 0x00,
	0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00,
	0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	0x00, 0x00, 0x00, 0x00, 0x1C,
}

// applyTimeMachineExclusion sets the documented Time Machine
// exclusion xattr on path with the canonical bplist value backupd
// expects. Writing a placeholder sentinel byte makes the doctor
// row look green but does NOT actually exclude the path: backupd
// parses the plist and ignores invalid encodings. The 61-byte
// payload here matches `tmutil addexclusion` exactly.
//
// Idempotent: re-setting an existing xattr succeeds silently.
func applyTimeMachineExclusion(path string) error {
	if err := unix.Setxattr(path, timeMachineExcludeXattr, timeMachineExcludeBplist, 0); err != nil {
		return fmt.Errorf("setxattr %s: %w", timeMachineExcludeXattr, err)
	}
	return nil
}
