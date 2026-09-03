//go:build windows

package main

import "errors"

const (
	netWMMoveResizeMove            = 8
	netWMMoveResizeSizeBottomRight = 4
)

func startX11MoveResize(uint32) error {
	return errors.New("X11 window control is not available on Windows")
}
