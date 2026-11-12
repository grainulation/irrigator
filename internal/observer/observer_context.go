package observer

import (
	"os"
	"syscall"
)

// obsCtxKey is a file-identity fingerprint (mtime, size, inode, device).
// budgetReadFile compares keys to detect that a provider-ceiling file changed
// or was replaced between reads. It is not a cache.
type obsCtxKey struct {
	mod  int64
	size int64
	ino  uint64
	dev  uint64
}

func obsCtxKeyOf(fi os.FileInfo) obsCtxKey {
	k := obsCtxKey{mod: fi.ModTime().UnixNano(), size: fi.Size()}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		k.ino, k.dev = uint64(st.Ino), uint64(st.Dev)
	}
	return k
}
