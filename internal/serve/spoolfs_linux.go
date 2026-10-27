//go:build linux

package serve

import "syscall"

// ramBacked reports whether path lives on a RAM-backed filesystem (tmpfs or
// ramfs), where "spooling to disk" in fact uses memory. An unknown or
// unreadable path reports false, since the result only decides whether to
// warn.
func ramBacked(path string) bool {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return false
	}
	// Statfs_t.Type is int64 on 64-bit targets and int32 on 32-bit ones. Compare
	// only the low 32 bits, so both build and RAMFS_MAGIC (which does not fit in
	// an int32) still matches.
	const tmpfsMagic, ramfsMagic = 0x01021994, 0x858458f6
	typ := uint32(st.Type)
	return typ == tmpfsMagic || typ == ramfsMagic
}
