package doctor

import (
	"errors"

	"golang.org/x/sys/unix"
)

// tmExclusionStatus reports whether path carries the documented Time
// Machine exclusion xattr that protects against the macOS Tahoe
// (26.x) backup-during-indexing freeze. The second return is false
// when the xattr is missing; the first holds the user-facing
// description for the doctor row.
func tmExclusionStatus(path string) (string, bool) {
	buf := make([]byte, 64)
	if _, err := unix.Getxattr(path, "com.apple.metadata:com_apple_backup_excludeItem", buf); err != nil {
		if errors.Is(err, unix.ENOATTR) {
			return "Time Machine exclusion xattr missing (macOS Tahoe may freeze during backup; run any maz command to self-heal)", false
		}
		return "Time Machine xattr probe failed: " + err.Error(), false
	}
	return "", true
}
