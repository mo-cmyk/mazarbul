//go:build linux

package capacity

import "syscall"

// freeDisk returns free-byte count of the filesystem holding path.
// Returns 0 on any error. On Linux, syscall.Statfs_t.Bsize is int64,
// so no conversion is needed; unconvert would flag a redundant cast.
func freeDisk(path string) int64 {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0
	}
	// #nosec G115 -- Bavail × Bsize cannot exceed int64 on any real fs.
	return int64(stat.Bavail) * stat.Bsize
}
