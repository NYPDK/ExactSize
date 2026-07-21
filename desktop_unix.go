//go:build !windows

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

var errNoDialog = errors.New("no supported native file dialog")

func hasNativeDialog() bool {
	for _, name := range []string{"kdialog", "zenity", "yad"} {
		if _, err := exec.LookPath(name); err == nil {
			return true
		}
	}
	return false
}

func openVideoDialog(startDir string) (string, bool, error) {
	if startDir == "" {
		startDir, _ = os.UserHomeDir()
	}
	filter := "Video files (*.mp4 *.mkv *.webm *.mov *.avi *.m4v *.mts *.m2ts *.ts *.wmv *.flv);;All files (*)"
	if path, err := exec.LookPath("kdialog"); err == nil {
		return runDialog(path, "--getopenfilename", startDir, filter, "--title", "Select input video")
	}
	if path, err := exec.LookPath("zenity"); err == nil {
		return runDialog(path, "--file-selection", "--title=Select input video", "--file-filter=Video files | *.mp4 *.mkv *.webm *.mov *.avi *.m4v *.mts *.m2ts *.ts *.wmv *.flv", "--file-filter=All files | *")
	}
	if path, err := exec.LookPath("yad"); err == nil {
		return runDialog(path, "--file-selection", "--title=Select input video")
	}
	return "", false, errNoDialog
}

func saveVideoDialog(suggested, container string) (string, bool, error) {
	if suggested == "" {
		home, _ := os.UserHomeDir()
		suggested = filepath.Join(home, "Videos", "compressed."+containerExtension(container))
	}
	ext := containerExtension(container)
	if path, err := exec.LookPath("kdialog"); err == nil {
		filter := strings.ToUpper(ext) + " video (*." + ext + ");;All files (*)"
		return runDialog(path, "--getsavefilename", suggested, filter, "--title", "Choose output file")
	}
	if path, err := exec.LookPath("zenity"); err == nil {
		return runDialog(path, "--file-selection", "--save", "--confirm-overwrite", "--title=Choose output file", "--filename="+suggested)
	}
	if path, err := exec.LookPath("yad"); err == nil {
		return runDialog(path, "--file-selection", "--save", "--confirm-overwrite", "--title=Choose output file", "--filename="+suggested)
	}
	return "", false, errNoDialog
}

func runDialog(command string, args ...string) (string, bool, error) {
	output, err := exec.Command(command, args...).Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() != 0 {
			return "", true, nil
		}
		return "", false, err
	}
	path := strings.TrimSpace(string(output))
	return path, path == "", nil
}

func revealFile(path string) error {
	if path == "" {
		return errors.New("no output file is available")
	}
	if _, err := os.Stat(path); err != nil {
		return errors.New("the output file no longer exists")
	}
	if runtime.GOOS != "linux" {
		return errors.New("revealing files is currently supported on Linux only")
	}
	if command, err := exec.LookPath("dbus-send"); err == nil {
		uri := "file://" + filepath.ToSlash(path)
		cmd := exec.Command(command, "--session", "--dest=org.freedesktop.FileManager1", "--type=method_call", "/org/freedesktop/FileManager1", "org.freedesktop.FileManager1.ShowItems", "array:string:"+uri, "string:")
		if err := cmd.Start(); err == nil {
			return nil
		}
	}
	command, err := exec.LookPath("xdg-open")
	if err != nil {
		return errors.New("xdg-open is not available")
	}
	return exec.Command(command, filepath.Dir(path)).Start()
}

func openExternalURL(rawURL string) error {
	if runtime.GOOS != "linux" {
		return errors.New("opening links is currently supported on Linux only")
	}
	command, err := exec.LookPath("xdg-open")
	if err != nil {
		return errors.New("xdg-open is not available")
	}
	return exec.Command(command, rawURL).Start()
}
