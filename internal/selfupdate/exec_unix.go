//go:build unix

package selfupdate

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

func execBinary(path string, argv, envv []string) error {
	return syscall.Exec(path, argv, envv)
}

// spawnDetached starts path in its own session and does not wait for it. The caller
// must be short-lived (or the child must be reparented) so the child is not left as a
// zombie; see spawnWatcher for the long-lived case.
func spawnDetached(path string, args ...string) error {
	cmd := exec.Command(path, args...)
	cmd.Stdin = nil
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd.Start()
}

// spawnWatcher starts the rollback watcher through a short-lived intermediate process
// ("--detach") and waits for that intermediate. The real watcher is thereby reparented to
// init (or systemd), which reaps it. Started directly, it would stay a zombie child of
// the re-exec'd Moorage process, whose Go runtime never waits for it.
func spawnWatcher(path string, args ...string) error {
	cmd := exec.Command(path, append(args, "--detach")...)
	cmd.Stdin = nil
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd.Run()
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func terminateProcess(pid int) {
	if pid > 0 {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
}

func killProcess(pid int) {
	if pid > 0 {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}

// chownLike gives path the owner and group of ref, best effort: it only succeeds when
// running as root (or as the owner, for a group the process is a member of).
func chownLike(path string, ref os.FileInfo) {
	if st, ok := ref.Sys().(*syscall.Stat_t); ok {
		_ = os.Lchown(path, int(st.Uid), int(st.Gid))
	}
}
