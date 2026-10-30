package kernel

// SetFsyncDirFn is a test-only hook that lets external tests inject a
// custom parent-directory fsync function. Used to simulate post-write
// durability failures (e.g., dir-fsync failing AFTER content fsync
// succeeded) without resorting to filesystem fault injection.
func (l *FileEventLog) SetFsyncDirFn(fn func(string) error) {
	l.fsyncDirFn = fn
}

// SetWriteOneFn is a test-only hook that replaces the per-bucket
// writeOne with a primitives-taking function. Used to simulate
// failure paths (pre-write OpenFile/Stat errors with observed=false,
// or post-write fsync/close errors with observed=true) without
// filesystem fault injection. The internal bucketPlan type is
// unexported, so the setter adapts to (path, buf, isNew) for external
// tests. The simulator additionally reports createdByUs, the runtime
// signal commitPlans relies on to decide whether rollback should
// remove (this writer created it) or truncate back (someone else owns
// the file).
func (l *FileEventLog) SetWriteOneFn(fn func(path string, buf []byte, isNew bool) (oldSize int64, observed bool, createdByUs bool, err error)) {
	l.writeOneFn = func(p bucketPlan) (int64, bool, bool, error) {
		return fn(p.path, p.buf, p.isNew)
	}
}
