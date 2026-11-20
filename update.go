package main

// appbird update: replace this binary with the latest GitHub release, after checking it against
// the release's checksums.txt. It only talks to GitHub; it never touches App Store Connect.

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const releaseRepo = "TweetingBirds/appbird"

// Overridable in tests.
var (
	executablePath = os.Executable
	updateHTTP     = &http.Client{Timeout: 2 * time.Minute}
)

// releasesAPI is the GitHub API URL for App Bird's repo. APPBIRD_UPDATE_API points it at a fake.
func releasesAPI() string {
	if v := os.Getenv("APPBIRD_UPDATE_API"); v != "" {
		return strings.TrimRight(v, "/")
	}
	return "https://api.github.com/repos/" + releaseRepo
}

type release struct {
	Tag    string `json:"tag_name"`
	URL    string `json:"html_url"`
	Assets []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

func (r *release) version() string { return strings.TrimPrefix(r.Tag, "v") }

func (r *release) asset(name string) string {
	for _, a := range r.Assets {
		if a.Name == name {
			return a.URL
		}
	}
	return ""
}

// fetchRelease reads the latest release, or the one tagged v<want>.
func fetchRelease(ctx context.Context, want string) (*release, error) {
	url := releasesAPI() + "/releases/latest"
	if want != "" {
		url = releasesAPI() + "/releases/tags/v" + strings.TrimPrefix(want, "v")
	}
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "appbird/"+version)
	if tok := os.Getenv("GITHUB_TOKEN"); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := updateHTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("couldn't reach GitHub: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound && want != "" {
		return nil, userErr("There's no App Bird release %s. See https://github.com/%s/releases", want, releaseRepo)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub answered %s for %s", resp.Status, url)
	}
	var r release
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&r); err != nil {
		return nil, fmt.Errorf("reading the release from GitHub: %w", err)
	}
	if r.Tag == "" {
		return nil, errors.New("GitHub's release has no tag")
	}
	return &r, nil
}

func download(url string, limit int64) ([]byte, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "appbird/"+version)
	resp, err := updateHTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("downloading %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("downloading %s: %s", url, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("downloading %s: %w", url, err)
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("downloading %s: bigger than %d MB", url, limit>>20)
	}
	return body, nil
}

// compareVersions compares dotted version numbers like 0.7.1. A pre-release suffix
// (0.8.0-rc1) sorts before the release itself.
func compareVersions(a, b string) int {
	split := func(v string) ([]int, string) {
		v = strings.TrimPrefix(v, "v")
		num, pre, _ := strings.Cut(v, "-")
		var parts []int
		for _, p := range strings.Split(num, ".") {
			n, _ := strconv.Atoi(p)
			parts = append(parts, n)
		}
		return parts, pre
	}
	an, ap := split(a)
	bn, bp := split(b)
	for i := 0; i < len(an) || i < len(bn); i++ {
		var x, y int
		if i < len(an) {
			x = an[i]
		}
		if i < len(bn) {
			y = bn[i]
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	switch {
	case ap == bp:
		return 0
	case ap == "":
		return 1
	case bp == "":
		return -1
	case ap < bp:
		return -1
	}
	return 1
}

func (a *app) cmdUpdate(o *options) (int, error) {
	if len(o.args) > 1 {
		return 2, userErr("Pass at most one version: appbird update [X.Y.Z] [--check]")
	}
	want := ""
	if len(o.args) == 1 {
		want = strings.TrimPrefix(o.args[0], "v")
		if !versionStringRE.MatchString(want) {
			return 2, userErr("%q isn't a version number. Try appbird update 0.7.1", o.args[0])
		}
	}
	if version == "dev" && want == "" {
		return 1, userErr("This appbird was built from source, so it doesn't know its version. Update it with\n" +
			"  go install github.com/" + releaseRepo + "@latest\nor install a release over it: appbird update <version>")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	rel, err := fetchRelease(ctx, want)
	if err != nil {
		return 1, err
	}
	target := rel.version()
	if want == "" && compareVersions(target, version) <= 0 {
		fmt.Fprintf(a.out, "appbird %s is the latest version.\n", version)
		return 0, nil
	}
	if want != "" && target == version {
		fmt.Fprintf(a.out, "appbird %s is already installed.\n", version)
		return 0, nil
	}
	if o.check {
		if want == "" {
			fmt.Fprintf(a.out, "appbird %s is out (you have %s). Run `appbird update` to install it.\n  %s\n", target, version, rel.URL)
		} else {
			fmt.Fprintf(a.out, "appbird %s exists (you have %s). Run `appbird update %s` to install it.\n", target, version, target)
		}
		return 0, nil
	}

	exe, err := executablePath()
	if err != nil {
		return 1, fmt.Errorf("finding the appbird binary: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	if strings.Contains(exe, "/Cellar/") {
		return 1, userErr("This appbird came from Homebrew (%s). Update it with brew upgrade appbird.", exe)
	}

	name := fmt.Sprintf("appbird_%s_%s_%s", target, runtime.GOOS, runtime.GOARCH)
	archive := name + ".tar.gz"
	if runtime.GOOS == "windows" {
		archive = name + ".zip"
	}
	archiveURL, sumsURL := rel.asset(archive), rel.asset("checksums.txt")
	if archiveURL == "" {
		return 1, userErr("Release %s has no build for %s/%s (%s).", target, runtime.GOOS, runtime.GOARCH, archive)
	}
	if sumsURL == "" {
		return 1, userErr("Release %s has no checksums.txt, so App Bird can't check the download. Nothing was changed.", target)
	}
	fmt.Fprintf(a.out, "Downloading appbird %s for %s/%s…\n", target, runtime.GOOS, runtime.GOARCH)
	sums, err := download(sumsURL, 1<<20)
	if err != nil {
		return 1, err
	}
	data, err := download(archiveURL, 200<<20)
	if err != nil {
		return 1, err
	}
	if err := verifyChecksum(sums, archive, data); err != nil {
		return 1, err
	}
	bin, err := extractBinary(archive, data)
	if err != nil {
		return 1, err
	}
	if err := replaceExecutable(exe, bin, target); err != nil {
		return 1, err
	}
	from := version
	if from == "dev" {
		from = "a source build"
	}
	fmt.Fprintf(a.out, "Updated %s from %s to %s.\n", exe, from, target)
	if rel.URL != "" {
		fmt.Fprintf(a.out, "What's new: %s\n", rel.URL)
	}
	return 0, nil
}

// verifyChecksum checks data against its line in a sha256sum-style checksums.txt.
func verifyChecksum(sums []byte, name string, data []byte) error {
	for _, line := range strings.Split(string(sums), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == name {
			got := sha256.Sum256(data)
			if !strings.EqualFold(fields[0], hex.EncodeToString(got[:])) {
				return userErr("%s doesn't match its checksum in checksums.txt. Nothing was changed.", name)
			}
			return nil
		}
	}
	return userErr("checksums.txt doesn't list %s, so App Bird can't check it. Nothing was changed.", name)
}

// extractBinary pulls the appbird binary out of a release archive.
func extractBinary(archive string, data []byte) ([]byte, error) {
	exe := "appbird"
	if strings.HasSuffix(archive, ".zip") {
		exe = "appbird.exe"
		zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return nil, fmt.Errorf("opening %s: %w", archive, err)
		}
		for _, f := range zr.File {
			if filepath.Base(f.Name) == exe && !f.FileInfo().IsDir() {
				rc, err := f.Open()
				if err != nil {
					return nil, err
				}
				defer rc.Close()
				return io.ReadAll(io.LimitReader(rc, 500<<20))
			}
		}
		return nil, fmt.Errorf("%s has no %s in it", archive, exe)
	}
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", archive, err)
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("%s has no %s in it", archive, exe)
		}
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", archive, err)
		}
		if h.Typeflag == tar.TypeReg && filepath.Base(h.Name) == exe {
			return io.ReadAll(io.LimitReader(tr, 500<<20))
		}
	}
}

// replaceExecutable writes bin next to exe, checks that it runs and reports the expected
// version, then moves it over exe. If anything fails, exe is left as it was.
func replaceExecutable(exe string, bin []byte, want string) error {
	dir := filepath.Dir(exe)
	tmp, err := os.CreateTemp(dir, ".appbird-update-*")
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			hint := "Run it again with sudo: sudo appbird update"
			if runtime.GOOS == "windows" {
				hint = "Run it again from a terminal opened as administrator."
			}
			return userErr("Can't write to %s. %s", dir, hint)
		}
		return err
	}
	tmpPath := tmp.Name()
	done := false
	defer func() {
		if !done {
			os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(bin); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	mode := os.FileMode(0o755)
	if info, err := os.Stat(exe); err == nil {
		mode = info.Mode().Perm() | 0o111
	}
	if err := os.Chmod(tmpPath, mode); err != nil {
		return err
	}
	runPath := tmpPath
	if runtime.GOOS == "windows" {
		// Windows only runs files that end in .exe.
		if err := os.Rename(tmpPath, tmpPath+".exe"); err != nil {
			return err
		}
		tmpPath += ".exe"
		runPath = tmpPath
	}
	out, err := exec.Command(runPath, "--version").Output()
	if err != nil || !strings.Contains(string(out), want) {
		return fmt.Errorf("the downloaded appbird didn't run (%v, %q). Nothing was changed", err, strings.TrimSpace(string(out)))
	}
	if runtime.GOOS == "windows" {
		// A running .exe can't be overwritten, but it can be renamed out of the way.
		old := exe + ".old"
		os.Remove(old)
		if err := os.Rename(exe, old); err != nil {
			return fmt.Errorf("moving the old appbird aside: %w", err)
		}
		if err := os.Rename(tmpPath, exe); err != nil {
			os.Rename(old, exe)
			return fmt.Errorf("installing the new appbird: %w", err)
		}
		done = true
		return nil
	}
	if err := os.Rename(tmpPath, exe); err != nil {
		return fmt.Errorf("installing the new appbird: %w", err)
	}
	done = true
	return nil
}

// ---- Daily "new version" notice ----

const updateCheckEvery = 24 * time.Hour

type updateCheckState struct {
	CheckedAt time.Time `json:"checked_at"`
	Latest    string    `json:"latest"`
}

func updateCheckPath() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "appbird", "update-check.json")
}

// wantUpdateNotice says whether this run may check for a new release: a release build, in a
// terminal, outside CI, not printing JSON, and not opted out with APPBIRD_NO_UPDATE_CHECK.
func wantUpdateNotice(cmd string, o *options, stdout, stderr io.Writer) bool {
	switch cmd {
	case "update", "version", "hoot", "--version":
		return false
	}
	if version == "dev" || o.json || os.Getenv("APPBIRD_NO_UPDATE_CHECK") != "" || os.Getenv("CI") != "" {
		return false
	}
	for _, w := range []io.Writer{stdout, stderr} {
		r, ok := w.(io.Reader)
		if !ok || !isTerminal(r) {
			return false
		}
	}
	return true
}

// startUpdateCheck looks up the latest release in the background, at most once a day, while
// the command runs. The function it returns waits briefly for the answer and prints a one-line
// notice when there's a newer version. statePath "" turns the check off.
func startUpdateCheck(statePath string, now time.Time) func(w io.Writer) {
	if statePath == "" {
		return func(io.Writer) {}
	}
	var st updateCheckState
	if raw, err := os.ReadFile(statePath); err == nil {
		json.Unmarshal(raw, &st)
	}
	if now.Sub(st.CheckedAt) < updateCheckEvery && !st.CheckedAt.After(now) {
		return func(io.Writer) {}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	result := make(chan string, 1)
	go func() {
		latest := ""
		if r, err := fetchRelease(ctx, ""); err == nil {
			latest = r.version()
		}
		result <- latest
	}()
	return func(w io.Writer) {
		defer cancel()
		var latest string
		select {
		case latest = <-result:
		case <-ctx.Done():
		}
		// Record the attempt even when GitHub didn't answer, so a slow network costs a
		// second or two once a day rather than on every command.
		raw, _ := json.Marshal(updateCheckState{CheckedAt: now, Latest: latest})
		os.MkdirAll(filepath.Dir(statePath), 0o755)
		os.WriteFile(statePath, raw, 0o644)
		if latest != "" && compareVersions(latest, version) > 0 {
			fmt.Fprintf(w, "\nappbird %s is out (you have %s). Run `appbird update` to install it.\n", latest, version)
		}
	}
}
