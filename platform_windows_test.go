//go:build windows

package main

import (
	"os/exec"
	"testing"
)

// Windows applies STARTUPINFO.wShowWindow to the first ShowWindow call a GUI
// process makes, ignoring the nCmdShow the process itself passes. A browser
// started with HideWindow therefore creates its app window hidden and can
// never reveal it, which strands ExactSize running with no visible UI.
func TestForegroundCommandLeavesTheWindowVisible(t *testing.T) {
	command := exec.Command("msedge.exe", "--app=http://127.0.0.1:1/")
	configureForegroundCommand(command)
	if command.SysProcAttr != nil && command.SysProcAttr.HideWindow {
		t.Fatal("configureForegroundCommand set HideWindow: the app window would start hidden")
	}
}

// The helpers we read output from must keep their console window suppressed,
// or every probe would flash a black window over the user's screen.
func TestBackgroundCommandSuppressesTheConsoleWindow(t *testing.T) {
	command := exec.Command("ffprobe.exe", "-version")
	configureBackgroundCommand(command)
	if command.SysProcAttr == nil || !command.SysProcAttr.HideWindow {
		t.Fatal("configureBackgroundCommand must hide the helper console window")
	}
}
