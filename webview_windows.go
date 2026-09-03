//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/jchv/go-webview2"
	"golang.org/x/sys/windows"
)

// ExactSize's native Windows shell: the UI runs inside a frameless window
// owned by this process, hosted by the WebView2 runtime that ships with
// Windows. Chromium's --app windows always draw their own title bar (they
// override WM_NCCALCSIZE), so a browser shell cannot be made frameless; a
// first-party window can, and it also keeps the app out of the browser's
// process tree and profile directories.
//
// The custom header drives move, resize, and minimize through
// /api/window/* exactly like the KDE build; the follower below tracks the
// cursor between the page's press and release notifications.

var (
	user32DLL             = windows.NewLazySystemDLL("user32.dll")
	getWindowLongPtrProc  = user32DLL.NewProc("GetWindowLongPtrW")
	setWindowLongPtrProc  = user32DLL.NewProc("SetWindowLongPtrW")
	setWindowPosProc      = user32DLL.NewProc("SetWindowPos")
	getCursorPosProc      = user32DLL.NewProc("GetCursorPos")
	getWindowRectProc     = user32DLL.NewProc("GetWindowRect")
	showWindowProc        = user32DLL.NewProc("ShowWindow")
	comctl32DLL           = windows.NewLazySystemDLL("comctl32.dll")
	setWindowSubclassProc = comctl32DLL.NewProc("SetWindowSubclass")
	defSubclassProcProc   = comctl32DLL.NewProc("DefSubclassProc")
)

const (
	gwlStyle        = ^uintptr(15)
	wsCaption       = 0x00C00000
	wmNCCalcSize    = 0x0083
	swpNoSize       = 0x0001
	swpNoMove       = 0x0002
	swpNoZOrder     = 0x0004
	swpNoActivate   = 0x0010
	swpFrameChanged = 0x0020
	swMinimize      = 6
)

type nativePoint struct{ X, Y int32 }

type nativeRect struct{ Left, Top, Right, Bottom int32 }

// appWindowHWND is nonzero while the WebView2 application window exists.
var appWindowHWND atomic.Uintptr

// launchWebViewWindow creates the frameless application window on a dedicated
// OS thread and runs its message loop. It reports whether the window was
// created; when the window later closes, onClosed runs once.
func launchWebViewWindow(url string, onClosed func()) bool {
	dataRoot := filepath.Join(os.Getenv("LOCALAPPDATA"), "ExactSizeWebView")
	// NOTE: do not pass --disable-gpu here. On the tested AMD/WebView2
	// stack, disabling GPU kills the browser process within ~30s of idle
	// (measured), taking the app with it; GPU compositing under concurrent
	// AMF encodes is handled by demoting encoders to BELOW_NORMAL priority
	// instead.
	created := make(chan bool, 1)
	go func() {
		runtime.LockOSThread()
		view := webview2.NewWithOptions(webview2.WebViewOptions{
			WindowOptions: webview2.WindowOptions{
				Title:  "ExactSize",
				Width:  minimumWindowWidth,
				Height: minimumWindowHeight,
				Center: true,
			},
			DataPath:  dataRoot,
			AutoFocus: true,
		})
		if view == nil {
			// No WebView2 runtime, or environment creation failed.
			created <- false
			return
		}
		hwnd := uintptr(view.Window())
		// Enforce the minimum size natively before the frame changes.
		view.SetSize(minimumWindowWidth, minimumWindowHeight, webview2.HintMin)
		makeFrameless(hwnd)
		appWindowHWND.Store(hwnd)
		view.Navigate(url)
		created <- true
		view.Run()
		appWindowHWND.Store(0)
		endWindowFollow()
		if onClosed != nil {
			onClosed()
		}
	}()
	select {
	case ok := <-created:
		return ok
	case <-time.After(20 * time.Second):
		return false
	}
}

// makeFrameless removes the caption while keeping the resize borders, the
// DWM shadow, and programmatic minimize support. The WM_NCCALCSIZE
// subclass then pulls the client area to the window's top edge, which
// removes the accent-colored frame line Windows otherwise draws there.
func makeFrameless(hwnd uintptr) {
	installFramelessFrame(hwnd)
	style, _, _ := getWindowLongPtrProc.Call(hwnd, uintptr(gwlStyle))
	setWindowLongPtrProc.Call(hwnd, uintptr(gwlStyle), style&^uintptr(wsCaption))
	setWindowPosProc.Call(hwnd, 0, 0, 0, 0, 0,
		swpFrameChanged|swpNoSize|swpNoMove|swpNoZOrder|swpNoActivate)
}

type ncCalcSizeParams struct {
	rgrc  [3]nativeRect
	lppos uintptr
}

var framelessSubclassProc uintptr

func installFramelessFrame(hwnd uintptr) {
	framelessSubclassProc = windows.NewCallback(func(window, msg, wp uintptr, lp unsafe.Pointer) uintptr {
		if msg == wmNCCalcSize && wp != 0 {
			params := (*ncCalcSizeParams)(lp)
			// rgrc[0] is the proposed client rect; rgrc[1] is the current
			// window rect. Keeping the window's own top edge removes the
			// reserved frame strip (and its colored line) at the top while
			// the side and bottom resize borders stay fully functional.
			params.rgrc[0].Top = params.rgrc[1].Top
			return 0
		}
		result, _, _ := defSubclassProcProc.Call(window, msg, wp, uintptr(lp))
		return result
	})
	// SetWindowSubclass must run on the window's own thread; the webview
	// goroutine creates the window and installs this before its message loop.
	setWindowSubclassProc.Call(hwnd, framelessSubclassProc, 1, 0)
}

func windowControlsSupported() bool {
	return appWindowHWND.Load() != 0
}

// The cursor follower: one active at a time, torn down by the matching
// *-end notification, a new follow, or the 60-second failsafe.
type windowFollow struct {
	stop chan struct{}
	done chan struct{}
}

var followMu sync.Mutex
var activeFollow *windowFollow

func startWindowFollow(resize bool) error {
	hwnd := appWindowHWND.Load()
	if hwnd == 0 {
		return errors.New("the application window is not available")
	}
	var cursor nativePoint
	getCursorPosProc.Call(uintptr(unsafe.Pointer(&cursor)))
	var bounds nativeRect
	getWindowRectProc.Call(hwnd, uintptr(unsafe.Pointer(&bounds)))

	followMu.Lock()
	if activeFollow != nil {
		close(activeFollow.stop)
	}
	follow := &windowFollow{stop: make(chan struct{}), done: make(chan struct{})}
	activeFollow = follow
	followMu.Unlock()
	followStart := time.Now()
	debugFollow := os.Getenv("EXACTSIZE_FOLLOW_LOG") != ""
	var logFile *os.File
	if debugFollow {
		logFile, _ = os.OpenFile(filepath.Join(os.TempDir(), "exactsize-follow.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	}
	logLine := func(line string) {
		if logFile != nil {
			_, _ = logFile.WriteString(line)
		}
	}

	go func() {
		defer close(follow.done)
		defer func() {
			if logFile != nil {
				_, _ = logFile.WriteString(fmt.Sprintf("follow ended t=%dms\n", time.Since(followStart).Milliseconds()))
				_ = logFile.Close()
			}
		}()
		failsafe := time.NewTimer(60 * time.Second)
		defer failsafe.Stop()
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-follow.stop:
				return
			case <-failsafe.C:
				return
			case <-ticker.C:
			}
			var now nativePoint
			getCursorPosProc.Call(uintptr(unsafe.Pointer(&now)))
			dx := int(now.X) - int(cursor.X)
			dy := int(now.Y) - int(cursor.Y)
			if debugFollow && (dx != 0 || dy != 0) {
				logLine(fmt.Sprintf("t=%dms cursor=%d,%d start=%d,%d d=%d,%d bounds=%d,%d,%d,%d resize=%v\n",
					time.Since(followStart).Milliseconds(), now.X, now.Y, cursor.X, cursor.Y, dx, dy,
					bounds.Left, bounds.Top, bounds.Right, bounds.Bottom, resize))
			}
			if resize {
				width := int(bounds.Right-bounds.Left) + dx
				height := int(bounds.Bottom-bounds.Top) + dy
				if width < minimumWindowWidth {
					width = minimumWindowWidth
				}
				if height < minimumWindowHeight {
					height = minimumWindowHeight
				}
				setWindowPosProc.Call(hwnd, 0,
					uintptr(int(bounds.Left)), uintptr(int(bounds.Top)),
					uintptr(width), uintptr(height),
					swpNoZOrder|swpNoActivate)
				continue
			}
			setWindowPosProc.Call(hwnd, 0,
				uintptr(int(bounds.Left)+dx), uintptr(int(bounds.Top)+dy),
				0, 0, swpNoSize|swpNoZOrder|swpNoActivate)
		}
	}()
	return nil
}

func startWindowDrag() error {
	return startWindowFollow(false)
}

func startWindowResize() error {
	return startWindowFollow(true)
}

func endWindowFollow() error {
	followMu.Lock()
	defer followMu.Unlock()
	if activeFollow != nil {
		close(activeFollow.stop)
		activeFollow = nil
	}
	return nil
}

func minimizeWindow() error {
	hwnd := appWindowHWND.Load()
	if hwnd == 0 {
		return errors.New("the application window is not available")
	}
	showWindowProc.Call(hwnd, swMinimize)
	return nil
}
