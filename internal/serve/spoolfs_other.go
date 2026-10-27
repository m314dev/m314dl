//go:build !linux

package serve

// ramBacked is detected only on Linux, where /tmp is often tmpfs. Other
// platforms never warn.
func ramBacked(string) bool { return false }
