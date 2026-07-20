package main

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
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
		_ = json.NewEncoder(w).Encode(map[string]string{"tag_name": "v1.10.0"})
	}))
	defer server.Close()

	info, err := checkLatestRelease(t.Context(), server.Client(), server.URL, "1.9.1")
	if err != nil {
		t.Fatal(err)
	}
	if !info.UpdateAvailable || info.LatestVersion != "1.10.0" || info.CurrentVersion != "1.9.1" {
		t.Fatalf("unexpected update info: %+v", info)
	}
	if info.ReleaseURL != exactSizeLatestReleasePage {
		t.Fatalf("release URL = %q", info.ReleaseURL)
	}
}

func TestUpdateRoutesCheckAndOpenLatestRelease(t *testing.T) {
	releaseServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"tag_name": "v9.0.0"})
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

	openRequest := httptest.NewRequest(http.MethodPost, "/api/update/open", nil)
	openRequest.Header.Set("X-ExactSize-Token", "secret")
	openResponse := httptest.NewRecorder()
	app.routes().ServeHTTP(openResponse, openRequest)
	if openResponse.Code != http.StatusOK {
		t.Fatalf("update open = %d %s", openResponse.Code, openResponse.Body.String())
	}
	if opened != exactSizeLatestReleasePage {
		t.Fatalf("opened URL = %q", opened)
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
	for _, behavior := range []string{"checkForUpdates", "/api/update/check", "/api/update/open", "updateAvailable"} {
		if !strings.Contains(string(script), behavior) {
			t.Fatalf("update UI is missing %q", behavior)
		}
	}
}
