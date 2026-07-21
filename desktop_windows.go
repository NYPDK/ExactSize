//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

var errNoDialog = errors.New("no supported native file dialog")

func windowsPowerShell() (string, error) {
	if path, err := exec.LookPath("powershell.exe"); err == nil {
		return path, nil
	}
	root := strings.TrimSpace(os.Getenv("SystemRoot"))
	if root != "" {
		candidate := filepath.Join(root, "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	return "", errNoDialog
}

func hasNativeDialog() bool {
	_, err := windowsPowerShell()
	return err == nil
}

func openVideoDialog(startDir string) (string, bool, error) {
	powershell, err := windowsPowerShell()
	if err != nil {
		return "", false, err
	}
	if startDir == "" {
		startDir, _ = os.UserHomeDir()
	}
	script := `[Console]::OutputEncoding = [System.Text.UTF8Encoding]::new($false)
Add-Type -AssemblyName System.Windows.Forms
$dialog = New-Object System.Windows.Forms.OpenFileDialog
$dialog.Title = 'Select input video'
$dialog.Filter = 'Video files|*.mp4;*.mkv;*.webm;*.mov;*.avi;*.m4v;*.mts;*.m2ts;*.ts;*.wmv;*.flv|All files|*.*'
$dialog.CheckFileExists = $true
$dialog.Multiselect = $false
if (Test-Path -LiteralPath $env:EXACTSIZE_DIALOG_DIR -PathType Container) { $dialog.InitialDirectory = $env:EXACTSIZE_DIALOG_DIR }
if ($dialog.ShowDialog() -eq [System.Windows.Forms.DialogResult]::OK) { [Console]::WriteLine($dialog.FileName); exit 0 }
exit 2`
	return runWindowsDialog(powershell, script, []string{"EXACTSIZE_DIALOG_DIR=" + filepath.FromSlash(startDir)})
}

func saveVideoDialog(suggested, container string) (string, bool, error) {
	powershell, err := windowsPowerShell()
	if err != nil {
		return "", false, err
	}
	ext := containerExtension(container)
	if suggested == "" {
		home, _ := os.UserHomeDir()
		suggested = filepath.Join(home, "Videos", "compressed."+ext)
	}
	script := `[Console]::OutputEncoding = [System.Text.UTF8Encoding]::new($false)
Add-Type -AssemblyName System.Windows.Forms
$dialog = New-Object System.Windows.Forms.SaveFileDialog
$dialog.Title = 'Choose output file'
$dialog.Filter = $env:EXACTSIZE_DIALOG_FILTER
$dialog.DefaultExt = $env:EXACTSIZE_DIALOG_EXT
$dialog.AddExtension = $true
$dialog.OverwritePrompt = $true
$dialog.InitialDirectory = [System.IO.Path]::GetDirectoryName($env:EXACTSIZE_DIALOG_PATH)
$dialog.FileName = [System.IO.Path]::GetFileName($env:EXACTSIZE_DIALOG_PATH)
if ($dialog.ShowDialog() -eq [System.Windows.Forms.DialogResult]::OK) { [Console]::WriteLine($dialog.FileName); exit 0 }
exit 2`
	environment := []string{
		"EXACTSIZE_DIALOG_PATH=" + filepath.FromSlash(suggested),
		"EXACTSIZE_DIALOG_EXT=" + ext,
		"EXACTSIZE_DIALOG_FILTER=" + strings.ToUpper(ext) + " video|*." + ext + "|All files|*.*",
	}
	return runWindowsDialog(powershell, script, environment)
}

func runWindowsDialog(powershell, script string, environment []string) (string, bool, error) {
	cmd := exec.Command(powershell, "-NoLogo", "-NoProfile", "-NonInteractive", "-STA", "-Command", script)
	cmd.Env = append(os.Environ(), environment...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	output, err := cmd.CombinedOutput()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 2 {
			return "", true, nil
		}
		message := strings.TrimSpace(string(output))
		if message == "" {
			message = err.Error()
		}
		return "", false, fmt.Errorf("open Windows file dialog: %s", message)
	}
	path := strings.TrimSpace(string(output))
	if path == "" {
		return "", true, nil
	}
	// The web UI treats paths as slash-separated; Windows and FFmpeg both
	// accept this form, while reveal/save convert it back before shell calls.
	return filepath.ToSlash(path), false, nil
}

func revealFile(path string) error {
	if path == "" {
		return errors.New("no output file is available")
	}
	path = filepath.FromSlash(path)
	if _, err := os.Stat(path); err != nil {
		return errors.New("the output file no longer exists")
	}
	explorer, err := exec.LookPath("explorer.exe")
	if err != nil {
		return errors.New("Windows Explorer is not available")
	}
	cmd := exec.Command(explorer, "/select,"+path)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	return cmd.Start()
}

func openExternalURL(rawURL string) error {
	launcher, err := exec.LookPath("rundll32.exe")
	if err != nil {
		return errors.New("the Windows URL launcher is not available")
	}
	cmd := exec.Command(launcher, "url.dll,FileProtocolHandler", rawURL)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	return cmd.Start()
}
