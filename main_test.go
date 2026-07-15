package main

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestMinimumWindowHeightFitsIndependentResolutionToggle(t *testing.T) {
	if minimumWindowHeight < 700 {
		t.Fatalf("minimum window height %d can reintroduce a vertical scrollbar", minimumWindowHeight)
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
