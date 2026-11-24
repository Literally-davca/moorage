//go:build linux

package main

import (
	"fmt"
	"os"
	"syscall"

	"moorage/internal/engine"
)

// runHostShellHelper runs inside the ephemeral moorage.role=host-shell container,
// chroots into the host root mounted at /host, and execs nsenter -t 1 -m -u -i -n -- <shell>
// (falling back to /bin/sh when /bin/bash is absent).
func runHostShellHelper(args []string) error {
	want := "bash"
	if len(args) > 0 && args[0] != "" {
		want = args[0]
	}
	shellPath, ok := engine.AllowedShells[want]
	if !ok {
		shellPath = "/bin/bash"
	}
	if err := syscall.Chroot("/host"); err != nil {
		return fmt.Errorf("chroot /host: %w", err)
	}
	if err := os.Chdir("/root"); err != nil {
		_ = os.Chdir("/")
	}
	if _, err := os.Stat(shellPath); err != nil {
		shellPath = "/bin/sh"
	}
	env := []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME=/root",
		"USER=root",
		"LOGNAME=root",
		"TERM=xterm-256color",
	}
	if marker := os.Getenv("MOORAGE_TERMINAL_SESSION"); marker != "" {
		env = append(env, "MOORAGE_TERMINAL_SESSION="+marker)
	}
	for _, nsenter := range []string{"/usr/bin/nsenter", "/bin/nsenter"} {
		if _, err := os.Stat(nsenter); err == nil {
			argv := []string{"nsenter", "-t", "1", "-m", "-u", "-i", "-n", "--", shellPath, "-l"}
			return syscall.Exec(nsenter, argv, env)
		}
	}
	return syscall.Exec(shellPath, []string{shellPath, "-l"}, env)
}
