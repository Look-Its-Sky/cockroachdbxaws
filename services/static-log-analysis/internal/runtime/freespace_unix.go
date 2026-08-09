//go:build unix

package runtime

import "syscall"

// freeSpace reports the bytes an unprivileged process may still write to the
// journal's filesystem. Bavail rather than Bfree: the reserved blocks a
// superuser could still use are not capacity this service has, and counting
// them would let the journal accept work it cannot store.
func freeSpace(path string) (uint64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, err
	}
	return uint64(stat.Bavail) * uint64(stat.Bsize), nil
}
