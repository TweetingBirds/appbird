package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// fakeReleases serves a GitHub release for tag with an archive whose appbird binary is a
// shell script that prints the version.
type fakeReleases struct {
	server   *httptest.Server
	tag      string
	badSum   bool
	noSums   bool
	requests int
}

func newFakeReleases(t *testing.T, tag string) *fakeReleases {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake release binary is a shell script")
	}
	f := &fakeReleases{tag: tag}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	t.Setenv("APPBIRD_UPDATE_API", f.server.URL)
	t.Setenv("GITHUB_TOKEN", "")
	return f
}

func (f *fakeReleases) name() string {
	return fmt.Sprintf("appbird_%s_%s_%s", strings.TrimPrefix(f.tag, "v"), runtime.GOOS, runtime.GOARCH)
}

func (f *fakeReleases) archive() []byte {
	script := "#!/bin/sh\necho appbird " + strings.TrimPrefix(f.tag, "v") + "\n"
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, file := range []struct{ name, body string }{{"README.md", "hi"}, {"appbird", script}} {
		tw.WriteHeader(&tar.Header{Name: f.name() + "/" + file.name, Mode: 0o755, Size: int64(len(file.body)), Typeflag: tar.TypeReg})
		tw.Write([]byte(file.body))
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func (f *fakeReleases) serve(w http.ResponseWriter, r *http.Request) {
	f.requests++
	archive := f.archive()
	switch r.URL.Path {
	case "/releases/latest", "/releases/tags/" + f.tag:
		assets := []map[string]string{{"name": f.name() + ".tar.gz", "browser_download_url": f.server.URL + "/dl/archive"}}
		if !f.noSums {
			assets = append(assets, map[string]string{"name": "checksums.txt", "browser_download_url": f.server.URL + "/dl/sums"})
		}
		json.NewEncoder(w).Encode(map[string]any{"tag_name": f.tag, "html_url": "https://example.com/" + f.tag, "assets": assets})
	case "/dl/archive":
		w.Write(archive)
	case "/dl/sums":
		sum := sha256.Sum256(archive)
		if f.badSum {
			sum[0]++
		}
		fmt.Fprintf(w, "%s  other.zip\n%s  %s.tar.gz\n", strings.Repeat("0", 64), hex.EncodeToString(sum[:]), f.name())
	default:
		http.NotFound(w, r)
	}
}

// installed makes a stand-in for the running binary and points the update at it.
func installed(t *testing.T, ver string) string {
	t.Helper()
	exe := filepath.Join(t.TempDir(), "appbird")
	os.WriteFile(exe, []byte("old binary"), 0o755)
	oldExe, oldVersion := executablePath, version
	executablePath = func() (string, error) { return exe, nil }
	version = ver
	t.Cleanup(func() { executablePath, version = oldExe, oldVersion })
	return exe
}

func runCLI(args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := run(args, strings.NewReader(""), &out, &errb)
	return code, out.String(), errb.String()
}

func TestUpdateInstallsTheLatestReleaseAfterCheckingIt(t *testing.T) {
	newFakeReleases(t, "v0.8.0")
	exe := installed(t, "0.7.1")
	code, out, errOut := runCLI("update")
	if code != 0 {
		t.Fatalf("code %d: %s %s", code, out, errOut)
	}
	if !strings.Contains(out, "from 0.7.1 to 0.8.0") || !strings.Contains(out, "https://example.com/v0.8.0") {
		t.Errorf("output: %s", out)
	}
	got, _ := os.ReadFile(exe)
	if !strings.Contains(string(got), "echo appbird 0.8.0") {
		t.Errorf("binary wasn't replaced: %q", got)
	}
	if info, _ := os.Stat(exe); info.Mode().Perm()&0o111 == 0 {
		t.Errorf("new binary isn't executable: %v", info.Mode())
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(exe), ".appbird-update-*")); len(left) > 0 {
		t.Errorf("left temp files: %v", left)
	}
}

func TestUpdateRefusesABadChecksumAndLeavesTheBinary(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*fakeReleases)
		want string
	}{
		{"mismatch", func(f *fakeReleases) { f.badSum = true }, "doesn't match its checksum"},
		{"missing", func(f *fakeReleases) { f.noSums = true }, "no checksums.txt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.set(newFakeReleases(t, "v0.8.0"))
			exe := installed(t, "0.7.1")
			code, _, errOut := runCLI("update")
			if code != 1 || !strings.Contains(errOut, tc.want) || !strings.Contains(errOut, "Nothing was changed") {
				t.Errorf("code %d: %s", code, errOut)
			}
			if got, _ := os.ReadFile(exe); string(got) != "old binary" {
				t.Errorf("binary changed: %q", got)
			}
		})
	}
}

func TestUpdateCheckOnlyReports(t *testing.T) {
	newFakeReleases(t, "v0.8.0")
	exe := installed(t, "0.7.1")
	code, out, _ := runCLI("update", "--check")
	if code != 0 || !strings.Contains(out, "appbird 0.8.0 is out (you have 0.7.1)") {
		t.Errorf("code %d: %s", code, out)
	}
	if got, _ := os.ReadFile(exe); string(got) != "old binary" {
		t.Errorf("--check changed the binary")
	}
}

func TestUpdateWhenAlreadyLatestOrFromSource(t *testing.T) {
	newFakeReleases(t, "v0.8.0")
	installed(t, "0.8.0")
	if code, out, _ := runCLI("update"); code != 0 || !strings.Contains(out, "0.8.0 is the latest") {
		t.Errorf("code %d: %s", code, out)
	}
	version = "dev"
	if code, _, errOut := runCLI("update"); code != 1 || !strings.Contains(errOut, "go install") {
		t.Errorf("code %d: %s", code, errOut)
	}
}

func TestUpdateToAChosenVersion(t *testing.T) {
	newFakeReleases(t, "v0.7.0")
	exe := installed(t, "0.7.1")
	if code, out, errOut := runCLI("update", "0.7.0"); code != 0 || !strings.Contains(out, "to 0.7.0") {
		t.Fatalf("code %d: %s %s", code, out, errOut)
	}
	if got, _ := os.ReadFile(exe); !strings.Contains(string(got), "0.7.0") {
		t.Errorf("binary: %q", got)
	}
	if code, _, errOut := runCLI("update", "0.6.9"); code != 1 || !strings.Contains(errOut, "no App Bird release 0.6.9") {
		t.Errorf("code %d: %s", code, errOut)
	}
}

func TestCompareVersions(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"0.7.1", "0.7.1", 0}, {"0.8.0", "0.7.1", 1}, {"0.7.1", "0.10.0", -1},
		{"1.0", "1.0.0", 0}, {"v1.2.0", "1.1.9", 1}, {"0.8.0-rc1", "0.8.0", -1}, {"0.8.0", "0.8.0-rc1", 1},
	} {
		if got := compareVersions(tc.a, tc.b); got != tc.want {
			t.Errorf("compareVersions(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestDailyUpdateNotice(t *testing.T) {
	f := newFakeReleases(t, "v0.8.0")
	installed(t, "0.7.1")
	state := filepath.Join(t.TempDir(), "appbird", "update-check.json")
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

	var w bytes.Buffer
	startUpdateCheck(state, now)(&w)
	if !strings.Contains(w.String(), "appbird 0.8.0 is out (you have 0.7.1). Run `appbird update`") {
		t.Errorf("first notice: %q", w.String())
	}
	// Within a day: no request and no notice.
	w.Reset()
	before := f.requests
	startUpdateCheck(state, now.Add(23*time.Hour))(&w)
	if w.Len() != 0 || f.requests != before {
		t.Errorf("checked again within a day: %q, %d requests", w.String(), f.requests-before)
	}
	// A day later it checks again; nothing to say once up to date.
	version = "0.8.0"
	startUpdateCheck(state, now.Add(25*time.Hour))(&w)
	if w.Len() != 0 || f.requests == before {
		t.Errorf("after a day: %q, %d requests", w.String(), f.requests-before)
	}
}

func TestNoUpdateNoticeOutsideATerminal(t *testing.T) {
	installed(t, "0.7.1")
	var out bytes.Buffer
	if wantUpdateNotice("status", &options{}, &out, &out) {
		t.Error("notice wanted for non-terminal output")
	}
	t.Setenv("CI", "true")
	if wantUpdateNotice("status", &options{}, os.Stdout, os.Stderr) {
		t.Error("notice wanted in CI")
	}
}
