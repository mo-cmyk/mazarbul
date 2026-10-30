package render

import "path"

// shardPath returns the relative path for a ULID-identified entity stub
// within the rendered/by-uid tree. The 3-char shard is ULID positions 0-2
// and the 2-char shard is positions 3-4. Crockford Base32 uppercase is
// preserved. Example: uid "01HZABCDEFGHJKMNPQRSTVWXYZ" produces
// "by-uid/01H/ZA/01HZABCDEFGHJKMNPQRSTVWXYZ.md".
func shardPath(uid string) string {
	if len(uid) < 5 {
		return path.Join("by-uid", "000", "00", uid+".md")
	}
	shard3 := uid[0:3]
	shard2 := uid[3:5]
	return path.Join("by-uid", shard3, shard2, uid+".md")
}
