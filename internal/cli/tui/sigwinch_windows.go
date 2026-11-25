//go:build windows

package tui

func watchResize(_ chan<- struct{}) func() {
	return func() {}
}
