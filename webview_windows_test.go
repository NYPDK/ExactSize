//go:build windows

package main

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var setCursorPosProc = user32DLL.NewProc("SetCursorPos")

func testWindowRect(t *testing.T, hwnd uintptr) nativeRect {
	t.Helper()
	var bounds nativeRect
	getWindowRectProc.Call(hwnd, uintptr(unsafe.Pointer(&bounds)))
	return bounds
}

func waitForAppWindow(t *testing.T, timeout time.Duration) uintptr {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if hwnd := appWindowHWND.Load(); hwnd != 0 {
			return hwnd
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the WebView2 application window never appeared")
	return 0
}

func moveTestCursor(t *testing.T, x, y int32) {
	t.Helper()
	setCursorPosProc.Call(uintptr(x), uintptr(y))
	time.Sleep(250 * time.Millisecond) // one follower tick plus slack
}

// TestWebViewWindowFramelessFollowResizeMinimizeClose covers the whole
// Windows window shell: the frameless style, the cursor-following move and
// resize, native minimize, and the close-to-shutdown wiring.
func TestWebViewWindowFramelessFollowResizeMinimizeClose(t *testing.T) {
	if testing.Short() {
		t.Skip("opens a real window on the desktop")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<!doctype html><title>ExactSize</title><body>shell test</body>"))
	}))
	defer server.Close()

	var closed atomic.Bool
	if !launchWebViewWindow(server.URL, func() { closed.Store(true) }) {
		t.Skip("WebView2 runtime is not available on this machine")
	}
	t.Cleanup(func() {
		postMessage(windows.NewLazySystemDLL("user32.dll").NewProc("PostMessageW"), appWindowHWND.Load())
		time.Sleep(300 * time.Millisecond)
	})
	hwnd := waitForAppWindow(t, 20*time.Second)
	t.Cleanup(func() { appWindowHWND.Store(0) })

	// Frameless: no caption, but resize borders and shadow frame remain.
	style, _, _ := getWindowLongPtrProc.Call(hwnd, uintptr(gwlStyle))
	if style&wsCaption != 0 {
		t.Fatalf("window style 0x%x still has WS_CAPTION", style)
	}

	// Move: the follower tracks the cursor from the press position. The
	// assertion compares against the cursor delta actually observed, so a
	// human wiggling the physical mouse mid-test cannot falsify it.
	origin := testWindowRect(t, hwnd)
	var press nativePoint
	getCursorPosProc.Call(uintptr(unsafe.Pointer(&press)))
	if err := startWindowDrag(); err != nil {
		t.Fatalf("startWindowDrag: %v", err)
	}
	moveTestCursor(t, press.X+140, press.Y+90)
	var release nativePoint
	getCursorPosProc.Call(uintptr(unsafe.Pointer(&release)))
	if err := endWindowFollow(); err != nil {
		t.Fatalf("endWindowFollow: %v", err)
	}
	moved := testWindowRect(t, hwnd)
	wantX := int(release.X - press.X)
	wantY := int(release.Y - press.Y)
	if abs(int(moved.Left-origin.Left)-wantX) > 10 || abs(int(moved.Top-origin.Top)-wantY) > 10 {
		t.Fatalf("drag moved window by %d,%d; cursor moved %d,%d",
			moved.Left-origin.Left, moved.Top-origin.Top, wantX, wantY)
	}

	// Resize: grows with the cursor, clamped at the minimum size.
	before := testWindowRect(t, hwnd)
	getCursorPosProc.Call(uintptr(unsafe.Pointer(&press)))
	if err := startWindowResize(); err != nil {
		t.Fatalf("startWindowResize: %v", err)
	}
	moveTestCursor(t, press.X+60, press.Y+40)
	getCursorPosProc.Call(uintptr(unsafe.Pointer(&release)))
	if err := endWindowFollow(); err != nil {
		t.Fatalf("endWindowFollow: %v", err)
	}
	after := testWindowRect(t, hwnd)
	wantWidth := int(release.X - press.X)
	wantHeight := int(release.Y - press.Y)
	if abs(int((after.Right-after.Left)-(before.Right-before.Left))-wantWidth) > 10 ||
		abs(int((after.Bottom-after.Top)-(before.Bottom-before.Top))-wantHeight) > 10 {
		t.Fatalf("resize changed size by %d,%d; cursor moved %d,%d",
			(after.Right-after.Left)-(before.Right-before.Left),
			(after.Bottom-after.Top)-(before.Bottom-before.Top), wantWidth, wantHeight)
	}

	// Minimize: a native iconic state.
	if err := minimizeWindow(); err != nil {
		t.Fatalf("minimizeWindow: %v", err)
	}
	isIconic := user32DLL.NewProc("IsIconic")
	iconic, _, _ := isIconic.Call(hwnd)
	if iconic == 0 {
		t.Fatal("window did not minimize")
	}
	restore := user32DLL.NewProc("ShowWindow")
	restore.Call(hwnd, 9) // SW_RESTORE

	// Close: WM_CLOSE runs the message loop out and fires onClosed.
	postClose := user32DLL.NewProc("PostMessageW")
	postClose.Call(hwnd, 0x0010, 0, 0) // WM_CLOSE
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && !closed.Load() {
		time.Sleep(100 * time.Millisecond)
	}
	if !closed.Load() {
		t.Fatal("closing the window never reached the shutdown callback")
	}
}

func postMessage(proc *windows.LazyProc, hwnd uintptr) {
	if hwnd != 0 {
		proc.Call(hwnd, 0x0010, 0, 0) // WM_CLOSE
	}
}

func abs(value int) int {
	if value < 0 {
		return -value
	}
	return value
}
