//go:build !windows

package tui

import (
	"os"
	"os/signal"
	"syscall"
)

func watchResize(ch chan<- struct{}) func() {
	sig := make(chan os.Signal, 2)
	signal.Notify(sig, syscall.SIGWINCH)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-sig:
				select {
				case ch <- struct{}{}:
				default:
				}
			case <-done:
				return
			}
		}
	}()
	return func() {
		signal.Stop(sig)
		close(done)
	}
}
