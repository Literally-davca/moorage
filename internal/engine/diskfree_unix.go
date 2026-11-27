//go:build !windows

package engine

import "syscall"

// diskFree returns the free bytes and percentage of the file system holding path
// (-1, -1 when unknown).
func diskFree(path string) (int64, float64) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil || st.Blocks == 0 {
		return -1, -1
	}
	free := int64(st.Bavail) * int64(st.Bsize)
	return free, float64(st.Bavail) / float64(st.Blocks) * 100
}

// diskTotalFree returns the size and free bytes of the file system holding path.
func diskTotalFree(path string) (int64, int64) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil || st.Blocks == 0 {
		return -1, -1
	}
	return int64(st.Blocks) * int64(st.Bsize), int64(st.Bavail) * int64(st.Bsize)
}
