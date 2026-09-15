//go:build !windows

package fsx

import "syscall"

// FreeSpace returns the bytes available to the current user on the volume that
// holds path.
func FreeSpace(path string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}
