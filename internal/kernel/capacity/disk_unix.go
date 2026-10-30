//go:build !windows && !linux

package capacity

import "syscall"

// freeDisk returns free-byte count of the filesystem holding path.
// Returns 0 on any error.
func freeDisk(path string) int64 {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0
	}
	// #nosec G115 -- Bavail × Bsize cannot exceed int64 on any real fs.
	return int64(stat.Bavail) * int64(stat.Bsize)
}
