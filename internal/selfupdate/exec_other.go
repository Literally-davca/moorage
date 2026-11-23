//go:build !unix

package selfupdate

import (
	"errors"
	"os"
	"os/exec"
)

func execBinary(_ string, _, _ []string) error {
	return errors.New("in-place exec is only supported on Unix systems")
}

func spawnDetached(path string, args ...string) error {
	cmd := exec.Command(path, args...)
	cmd.Stdin = nil
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Start()
}

func spawnWatcher(path string, args ...string) error {
	return spawnDetached(path, args...)
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	_, err := os.FindProcess(pid)
	return err == nil
}

func terminateProcess(pid int) {
	killProcess(pid)
}

func killProcess(pid int) {
	if pid > 0 {
		if p, err := os.FindProcess(pid); err == nil {
			_ = p.Kill()
		}
	}
}

func chownLike(string, os.FileInfo) {}
