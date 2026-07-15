package main

import (
	"errors"
	"strings"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"
)

// The app window runs on XWayland specifically so that window dragging can be
// a genuine compositor move: X11 clients start one by sending the root window
// a _NET_WM_MOVERESIZE client message, which KWin treats exactly like a title
// bar drag, including the user's move and resize effects. Wayland offers no
// equivalent to external processes.
const (
	netWMMoveResizeMove            = 8
	netWMMoveResizeSizeBottomRight = 4
)

// startX11MoveResize finds the app window by class and asks the window
// manager to start an interactive move or resize anchored at the pointer.
func startX11MoveResize(direction uint32) error {
	conn, err := xgb.NewConn()
	if err != nil {
		return err
	}
	defer conn.Close()

	root := xproto.Setup(conn).DefaultScreen(conn).Root
	window, err := findX11WindowByClass(conn, root, "exactsize")
	if err != nil {
		return err
	}
	pointer, err := xproto.QueryPointer(conn, root).Reply()
	if err != nil {
		return err
	}
	moveResizeAtom, err := internAtom(conn, "_NET_WM_MOVERESIZE")
	if err != nil {
		return err
	}

	event := xproto.ClientMessageEvent{
		Format: 32,
		Window: window,
		Type:   moveResizeAtom,
		Data: xproto.ClientMessageDataUnionData32New([]uint32{
			uint32(pointer.RootX),
			uint32(pointer.RootY),
			direction,
			1, // left button
			1, // source: normal application
		}),
	}
	return xproto.SendEventChecked(conn, false, root,
		xproto.EventMaskSubstructureRedirect|xproto.EventMaskSubstructureNotify,
		string(event.Bytes())).Check()
}

func internAtom(conn *xgb.Conn, name string) (xproto.Atom, error) {
	reply, err := xproto.InternAtom(conn, false, uint16(len(name)), name).Reply()
	if err != nil {
		return 0, err
	}
	return reply.Atom, nil
}

func findX11WindowByClass(conn *xgb.Conn, root xproto.Window, class string) (xproto.Window, error) {
	clientList, err := internAtom(conn, "_NET_CLIENT_LIST")
	if err != nil {
		return 0, err
	}
	prop, err := xproto.GetProperty(conn, false, root, clientList, xproto.AtomWindow, 0, 1<<16).Reply()
	if err != nil {
		return 0, err
	}
	for offset := 0; offset+4 <= len(prop.Value); offset += 4 {
		window := xproto.Window(uint32(prop.Value[offset]) | uint32(prop.Value[offset+1])<<8 |
			uint32(prop.Value[offset+2])<<16 | uint32(prop.Value[offset+3])<<24)
		wmClass, err := xproto.GetProperty(conn, false, window, xproto.AtomWmClass, xproto.AtomString, 0, 1024).Reply()
		if err != nil {
			continue
		}
		if strings.Contains(strings.ToLower(string(wmClass.Value)), class) {
			return window, nil
		}
	}
	return 0, errors.New("the app window was not found on the X server")
}
