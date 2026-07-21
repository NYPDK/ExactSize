package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

func TestIsNewerRelease(t *testing.T) {
	tests := []struct {
		name    string
		current string
		latest  string
		want    bool
	}{
		{name: "new patch", current: "1.9.1", latest: "v1.9.2", want: true},
		{name: "new minor", current: "1.9.9", latest: "v1.10.0", want: true},
		{name: "new major", current: "1.99.99", latest: "2.0.0", want: true},
		{name: "same", current: "1.9.1", latest: "v1.9.1", want: false},
		{name: "older", current: "1.9.2", latest: "v1.9.1", want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := isNewerRelease(test.current, test.latest)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("isNewerRelease(%q, %q) = %v, want %v", test.current, test.latest, got, test.want)
			}
		})
	}
	if _, err := isNewerRelease("1.9.1", "nightly"); err == nil {
		t.Fatal("malformed release tags must be rejected")
	}
	if _, err := isNewerRelease("1.9.1", "1.10.0-beta.1"); err == nil {
		t.Fatal("prerelease tags must not be treated as stable releases")
	}
}

func TestCheckLatestRelease(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Accept"); got != "application/vnd.github+json" {
			t.Errorf("Accept = %q", got)
		}
		if got := r.Header.Get("X-GitHub-Api-Version"); got != "2022-11-28" {
			t.Errorf("X-GitHub-Api-Version = %q", got)
		}
		if got := r.Header.Get("User-Agent"); got != "ExactSize/1.9.1" {
			t.Errorf("User-Agent = %q", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tag_name": "v1.10.0",
			"html_url": "https://github.com/NYPDK/ExactSize/releases/tag/v1.10.0",
			"assets": []map[string]any{{
				"name":                 "ExactSize-1.10.0-x86_64.AppImage",
				"state":                "uploaded",
				"size":                 123456,
				"digest":               "sha256:" + strings.Repeat("a", 64),
				"browser_download_url": "https://github.com/NYPDK/ExactSize/releases/download/v1.10.0/ExactSize-1.10.0-x86_64.AppImage",
			}},
		})
	}))
	defer server.Close()

	info, err := checkLatestRelease(t.Context(), server.Client(), server.URL, "1.9.1")
	if err != nil {
		t.Fatal(err)
	}
	if !info.UpdateAvailable || info.LatestVersion != "1.10.0" || info.CurrentVersion != "1.9.1" {
		t.Fatalf("unexpected update info: %+v", info)
	}
	if info.ReleaseURL != "https://github.com/NYPDK/ExactSize/releases/tag/v1.10.0" {
		t.Fatalf("release URL = %q", info.ReleaseURL)
	}
	if err := validateUpdateAsset(info); err != nil {
		t.Fatalf("valid exact release asset was rejected: %v", err)
	}
}

func TestUpdateRoutesCheckAndOpenExactReleaseAsset(t *testing.T) {
	assetURL := "https://github.com/NYPDK/ExactSize/releases/download/v9.0.0/ExactSize-9.0.0-x86_64.AppImage"
	releaseServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tag_name": "v9.0.0",
			"html_url": "https://github.com/NYPDK/ExactSize/releases/tag/v9.0.0",
			"assets": []map[string]any{{
				"name":                 "ExactSize-9.0.0-x86_64.AppImage",
				"state":                "uploaded",
				"size":                 123456,
				"digest":               "sha256:" + strings.Repeat("b", 64),
				"browser_download_url": assetURL,
			}},
		})
	}))
	defer releaseServer.Close()

	web := fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("ExactSize")}}
	app := newApp("ffmpeg", "ffprobe", "secret", fs.FS(web))
	app.updateClient = releaseServer.Client()
	app.releaseAPIURL = releaseServer.URL
	opened := ""
	app.openURL = func(url string) error {
		opened = url
		return nil
	}

	checkRequest := httptest.NewRequest(http.MethodGet, "/api/update/check", nil)
	checkRequest.Header.Set("X-ExactSize-Token", "secret")
	checkResponse := httptest.NewRecorder()
	app.routes().ServeHTTP(checkResponse, checkRequest)
	if checkResponse.Code != http.StatusOK || !strings.Contains(checkResponse.Body.String(), `"updateAvailable":true`) {
		t.Fatalf("update check = %d %s", checkResponse.Code, checkResponse.Body.String())
	}

	openRequest := httptest.NewRequest(http.MethodPost, "/api/update/open-asset", nil)
	openRequest.Header.Set("X-ExactSize-Token", "secret")
	openResponse := httptest.NewRecorder()
	app.routes().ServeHTTP(openResponse, openRequest)
	if openResponse.Code != http.StatusOK {
		t.Fatalf("update open = %d %s", openResponse.Code, openResponse.Body.String())
	}
	if opened != assetURL {
		t.Fatalf("opened URL = %q", opened)
	}
}

func TestValidateUpdateAssetRejectsUntrustedOrIncompleteMetadata(t *testing.T) {
	valid := UpdateInfo{
		LatestVersion: "2.0.0",
		AssetName:     "ExactSize-2.0.0-x86_64.AppImage",
		AssetURL:      "https://github.com/NYPDK/ExactSize/releases/download/v2.0.0/ExactSize-2.0.0-x86_64.AppImage",
		AssetSize:     100,
		AssetDigest:   "sha256:" + strings.Repeat("c", 64),
		tagName:       "v2.0.0",
	}
	if err := validateUpdateAsset(valid); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*UpdateInfo){
		"wrong host":     func(info *UpdateInfo) { info.AssetURL = "https://example.com/" + info.AssetName },
		"wrong asset":    func(info *UpdateInfo) { info.AssetName = "other.AppImage" },
		"missing digest": func(info *UpdateInfo) { info.AssetDigest = "" },
		"invalid size":   func(info *UpdateInfo) { info.AssetSize = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := valid
			mutate(&candidate)
			if err := validateUpdateAsset(candidate); err == nil {
				t.Fatal("invalid update asset was accepted")
			}
		})
	}
}

func fakeAppImage(payload string) []byte {
	data := make([]byte, 11, 11+len(payload))
	copy(data[:4], "\x7fELF")
	copy(data[8:11], "AI\x02")
	return append(data, payload...)
}

func TestDownloadAndReplaceAppImageAtomicallyAfterVerification(t *testing.T) {
	oldImage := fakeAppImage("working old version")
	newImage := fakeAppImage(strings.Repeat("verified new version", 32))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(newImage)
	}))
	defer server.Close()

	current := filepath.Join(t.TempDir(), "ExactSize.AppImage")
	if err := os.WriteFile(current, oldImage, 0o751); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(newImage)
	asset := UpdateInfo{AssetURL: server.URL, AssetSize: int64(len(newImage)), AssetDigest: fmt.Sprintf("sha256:%x", digest)}
	var progress int64
	if err := downloadAndReplaceAppImage(t.Context(), server.Client(), current, asset, func(value int64) { progress = value }); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(current)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, newImage) {
		t.Fatal("installed AppImage does not match the verified download")
	}
	if progress != int64(len(newImage)) {
		t.Fatalf("progress = %d, want %d", progress, len(newImage))
	}
	info, err := os.Stat(current)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o751 {
		t.Fatalf("installed mode = %v, want 0751", info.Mode().Perm())
	}
	assertNoUpdateTemporaryFiles(t, filepath.Dir(current))
}

func TestDownloadVerificationFailuresLeaveCurrentAppImageUntouched(t *testing.T) {
	oldImage := fakeAppImage("known working version")
	newImage := fakeAppImage(strings.Repeat("download", 16))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(newImage)
	}))
	defer server.Close()
	goodDigest := sha256.Sum256(newImage)
	for name, asset := range map[string]UpdateInfo{
		"digest mismatch": {AssetURL: server.URL, AssetSize: int64(len(newImage)), AssetDigest: "sha256:" + strings.Repeat("0", 64)},
		"truncated":       {AssetURL: server.URL, AssetSize: int64(len(newImage) + 1), AssetDigest: fmt.Sprintf("sha256:%x", goodDigest)},
		"oversized":       {AssetURL: server.URL, AssetSize: int64(len(newImage) - 1), AssetDigest: fmt.Sprintf("sha256:%x", goodDigest)},
	} {
		t.Run(name, func(t *testing.T) {
			directory := t.TempDir()
			current := filepath.Join(directory, "ExactSize.AppImage")
			if err := os.WriteFile(current, oldImage, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := downloadAndReplaceAppImage(t.Context(), server.Client(), current, asset, nil); err == nil {
				t.Fatal("invalid download unexpectedly replaced the AppImage")
			}
			got, err := os.ReadFile(current)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, oldImage) {
				t.Fatal("working AppImage changed after a failed update")
			}
			assertNoUpdateTemporaryFiles(t, directory)
		})
	}
}

func TestRunningAppImagePathResolvesOnlyARealAppImageBuild(t *testing.T) {
	directory := t.TempDir()
	image := filepath.Join(directory, "ExactSize-2.0.0-x86_64.AppImage")
	if err := os.WriteFile(image, fakeAppImage("running"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "ExactSize.AppImage")
	if err := os.Symlink(image, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("APPDIR", filepath.Join(directory, "mounted-appdir"))
	t.Setenv("APPIMAGE", link)
	got, err := runningAppImagePath()
	if err != nil {
		t.Fatal(err)
	}
	if got != image {
		t.Fatalf("runningAppImagePath = %q, want resolved path %q", got, image)
	}
	t.Setenv("APPDIR", "")
	if _, err := runningAppImagePath(); err == nil {
		t.Fatal("source builds must not be treated as self-updatable AppImages")
	}
}

func TestReleaseScriptsRequireTheAppImageAndZsyncPair(t *testing.T) {
	buildScript, err := os.ReadFile("scripts/build-appimage.sh")
	if err != nil {
		t.Fatal(err)
	}
	publishScript, err := os.ReadFile("scripts/publish-release.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"gh-releases-zsync|NYPDK|ExactSize|latest|ExactSize-*-x86_64.AppImage.zsync",
		"--updateinformation",
		"--file-url",
		`[ -s "$ZSYNC_OUTPUT" ]`,
	} {
		if !strings.Contains(string(buildScript), required) {
			t.Fatalf("AppImage build script is missing %q", required)
		}
	}
	for _, required := range []string{`gh release create "$TAG" "$APPIMAGE" "$ZSYNC"`, `basename "$APPIMAGE"`, `basename "$ZSYNC"`} {
		if !strings.Contains(string(publishScript), required) {
			t.Fatalf("release publishing script is missing %q", required)
		}
	}
}

func assertNoUpdateTemporaryFiles(t *testing.T, directory string) {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".update-") {
			t.Fatalf("temporary update file was not removed: %s", entry.Name())
		}
	}
}

func TestUpdateIndicatorIsWiredIntoTheHeader(t *testing.T) {
	markup, err := webAssets.ReadFile("web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	script, err := webAssets.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	styles, err := webAssets.ReadFile("web/styles.css")
	if err != nil {
		t.Fatal(err)
	}
	for label, source := range map[string]string{
		"markup":     string(markup),
		"javascript": string(script),
		"styles":     string(styles),
	} {
		if !strings.Contains(source, "updateButton") && !strings.Contains(source, ".update-button") {
			t.Fatalf("%s does not include the update indicator", label)
		}
	}
	combinedUI := string(markup) + string(script)
	for _, behavior := range []string{"checkForUpdates", "/api/update/check", "/api/update/open-asset", "/api/update/install", "/api/update/status", "updateAvailable", "Open exact AppImage asset", "renderInstalledUpdate", `textContent = "Done"`, "closeUpdateDialog();"} {
		if !strings.Contains(combinedUI, behavior) {
			t.Fatalf("update UI is missing %q", behavior)
		}
	}
}

func TestDropCapturesProtectedDataBeforeAwaitAndSupportsLinuxURIFlavors(t *testing.T) {
	script, err := webAssets.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	source := string(script)
	start := strings.Index(source, "async function handleDrop")
	end := strings.Index(source[start:], "async function uploadInput")
	if start < 0 || end < 0 {
		t.Fatal("could not find handleDrop implementation")
	}
	handler := source[start : start+end]
	fileCapture := strings.Index(handler, "const [file] = dataTransfer.files")
	firstAwait := strings.Index(handler, "if (await")
	if fileCapture < 0 || firstAwait < 0 || fileCapture > firstAwait {
		t.Fatal("the dropped File must be captured synchronously before the first await")
	}
	for _, flavor := range []string{"text/uri-list", "text/plain", "x-special/gnome-copied-files"} {
		if !strings.Contains(handler, flavor) {
			t.Fatalf("drop handler does not read %q", flavor)
		}
	}
	for _, guard := range []string{`uri.protocol !== "file:"`, `uri.hostname !== "localhost"`} {
		if !strings.Contains(source, guard) {
			t.Fatalf("drop URI parsing is missing %q", guard)
		}
	}
}
