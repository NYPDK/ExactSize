package main

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestInstanceGuardRejectsDuplicateAndReleasesOnClose(t *testing.T) {
	guardName := "\x00exactsize-test-" + strconv.Itoa(os.Getpid()) + "-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	first, err := acquireInstanceGuard(guardName)
	if err != nil {
		t.Fatalf("acquire first instance guard: %v", err)
	}
	defer first.Close()

	if duplicate, err := acquireInstanceGuard(guardName); !errors.Is(err, errAlreadyRunning) {
		if duplicate != nil {
			duplicate.Close()
		}
		t.Fatalf("duplicate instance guard error = %v, want %v", err, errAlreadyRunning)
	}

	if err := first.Close(); err != nil {
		t.Fatalf("close first instance guard: %v", err)
	}
	reopened, err := acquireInstanceGuard(guardName)
	if err != nil {
		t.Fatalf("reacquire released instance guard: %v", err)
	}
	reopened.Close()
}

func TestAlreadyRunningDialogUsesNativeWarning(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "dialog-args")
	kdialog := filepath.Join(dir, "kdialog")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$EXACTSIZE_DIALOG_TEST_LOG\"\n"
	if err := os.WriteFile(kdialog, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("EXACTSIZE_DIALOG_TEST_LOG", logPath)

	showWarningDialog(alreadyRunningMessage)

	args, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"--sorry", alreadyRunningMessage, "--title", "ExactSize"} {
		if !strings.Contains(string(args), expected) {
			t.Errorf("warning dialog arguments are missing %q:\n%s", expected, args)
		}
	}
}

func TestMinimumWindowHeightFitsIndependentResolutionToggle(t *testing.T) {
	if minimumWindowHeight < 700 {
		t.Fatalf("minimum window height %d can reintroduce a vertical scrollbar", minimumWindowHeight)
	}
}

func TestAppImageIntegrationRetargetsLaunchersToCurrentVersion(t *testing.T) {
	home := t.TempDir()
	desktopDir := filepath.Join(home, "Desktop")
	if err := os.MkdirAll(desktopDir, 0o755); err != nil {
		t.Fatal(err)
	}
	shortcut := filepath.Join(desktopDir, "exactsize.desktop")
	if err := os.WriteFile(shortcut, []byte("[Desktop Entry]\nName=ExactSize\nStartupWMClass=ExactSize\nExec=/old/ExactSize.AppDir/AppRun\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	unrelated := filepath.Join(desktopDir, "other.desktop")
	if err := os.WriteFile(unrelated, []byte("[Desktop Entry]\nName=Other\nExec=/other\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	first := filepath.Join(home, "ExactSize-previous.AppImage")
	current := filepath.Join(home, "ExactSize-current.AppImage")
	for _, path := range []string{first, current} {
		if err := os.WriteFile(path, []byte("appimage"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := installAppImageIntegration(first, home); err != nil {
		t.Fatalf("install previous AppImage integration: %v", err)
	}
	if err := installAppImageIntegration(current, home); err != nil {
		t.Fatalf("retarget current AppImage integration: %v", err)
	}

	stable := filepath.Join(home, ".local", "share", "exactsize", "ExactSize.AppImage")
	resolved, err := filepath.EvalSymlinks(stable)
	if err != nil {
		t.Fatalf("resolve stable AppImage link: %v", err)
	}
	if resolved != current {
		t.Fatalf("stable AppImage resolves to %q, want current %q", resolved, current)
	}
	for _, path := range []string{
		filepath.Join(home, ".local", "share", "applications", "io.exactsize.ExactSize.desktop"),
		shortcut,
	} {
		entry, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read launcher %s: %v", path, err)
		}
		for _, expected := range []string{`Exec="` + stable + `"`, "X-AppImage-Version=" + version} {
			if !strings.Contains(string(entry), expected) {
				t.Errorf("launcher %s is missing %q:\n%s", path, expected, entry)
			}
		}
	}
	unrelatedData, err := os.ReadFile(unrelated)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(unrelatedData), stable) {
		t.Fatal("unrelated desktop shortcut was modified")
	}

	// Launching through the stable link must preserve its real target instead
	// of accidentally replacing it with a self-referential symlink.
	if err := installAppImageIntegration(stable, home); err != nil {
		t.Fatalf("refresh integration through stable link: %v", err)
	}
	resolved, err = filepath.EvalSymlinks(stable)
	if err != nil || resolved != current {
		t.Fatalf("stable launch link after refresh = %q, %v; want %q", resolved, err, current)
	}
}

func TestBrowserProfileCleanup(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("TMPDIR", tempDir)

	profileDir, cleanup, err := createBrowserProfile()
	if err != nil {
		t.Fatalf("createBrowserProfile: %v", err)
	}
	owner, err := os.ReadFile(filepath.Join(profileDir, browserProfileOwner))
	if err != nil {
		t.Fatalf("read owner marker: %v", err)
	}
	if string(owner) != strconv.Itoa(os.Getpid()) {
		t.Fatalf("owner marker = %q, want %d", owner, os.Getpid())
	}

	cleanup()
	cleanup()
	if _, err := os.Stat(profileDir); !os.IsNotExist(err) {
		t.Fatalf("profile directory still exists after cleanup: %v", err)
	}
}

func TestStaleBrowserProfileCleanupPreservesActiveProfiles(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("TMPDIR", tempDir)

	makeProfile := func(name, owner string) string {
		t.Helper()
		dir := filepath.Join(tempDir, browserProfilePrefix+name)
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatalf("create profile %s: %v", name, err)
		}
		if owner != "" {
			if err := os.WriteFile(filepath.Join(dir, browserProfileOwner), []byte(owner), 0o600); err != nil {
				t.Fatalf("write profile owner %s: %v", name, err)
			}
		}
		return dir
	}

	active := makeProfile("active", strconv.Itoa(os.Getpid()))
	stale := makeProfile("stale", strconv.Itoa(1<<30))
	legacyRecent := makeProfile("legacy-recent", "")
	legacyOld := makeProfile("legacy-old", "")
	oldTime := time.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(legacyOld, oldTime, oldTime); err != nil {
		t.Fatalf("age legacy profile: %v", err)
	}

	cleanupStaleBrowserProfiles()

	for _, path := range []string{active, legacyRecent} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("profile %s should have been preserved: %v", path, err)
		}
	}
	for _, path := range []string{stale, legacyOld} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("profile %s should have been removed: %v", path, err)
		}
	}
}
