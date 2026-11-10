package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Apple re-encodes screenshots and previews, so a downloaded file never has the checksum of the
// file that was uploaded. `media pull` records, next to the files it writes, which upload each
// one stands for. A file that's still byte-for-byte what pull wrote is matched by that upload's
// checksum; once you change it, its own checksum counts.
const sourcesFile = ".appbird-sources.json"

var sourcesCache = map[string]map[string]string{}

func readSources(dir string) map[string]string {
	if m, ok := sourcesCache[dir]; ok {
		return m
	}
	m := map[string]string{}
	if raw, err := os.ReadFile(filepath.Join(dir, sourcesFile)); err == nil {
		_ = json.Unmarshal(raw, &m)
	}
	sourcesCache[dir] = m
	return m
}

// sourceChecksum is the checksum App Store Connect knows a local file by.
func sourceChecksum(path string) (string, error) {
	sum, err := fileMD5(path)
	if err != nil {
		return "", err
	}
	if src := readSources(filepath.Dir(path))[sum]; src != "" {
		return src, nil
	}
	return sum, nil
}

// Download GETs a file from Apple's CDN (no bearer token), retrying network errors and 5xx.
func (c *Client) Download(u string) ([]byte, string, error) {
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		resp, err := c.http.Get(u)
		if err == nil {
			data, readErr := io.ReadAll(resp.Body)
			resp.Body.Close()
			switch {
			case readErr != nil:
				err = readErr
			case resp.StatusCode < 300:
				return data, resp.Header.Get("Content-Type"), nil
			case resp.StatusCode < 500:
				return nil, "", userErr("downloading %s failed with HTTP %d", u, resp.StatusCode)
			default:
				err = fmt.Errorf("HTTP %d", resp.StatusCode)
			}
		}
		lastErr = err
		time.Sleep(retryDelay << attempt)
	}
	return nil, "", userErr("downloading %s failed: %v", u, lastErr)
}

var orderPrefixRE = regexp.MustCompile(`^\d+[-_ ]+`)

// pulledName is the file name pull writes for the i-th (0-based) item of a set.
func pulledName(i int, fileName, defaultExt string, allowed map[string]bool) string {
	base := filepath.Base(fileName)
	if base == "." || base == "/" || base == "" {
		base = "item"
	}
	ext := strings.ToLower(filepath.Ext(base))
	stem := strings.TrimSuffix(base, filepath.Ext(base))
	if !allowed[ext] {
		ext = defaultExt
	}
	stem = orderPrefixRE.ReplaceAllString(stem, "")
	if stem == "" {
		stem = "item"
	}
	return fmt.Sprintf("%02d-%s%s", i+1, stem, ext)
}

func screenshotURL(item *Resource, ext string) string {
	asset, _ := item.Attributes["imageAsset"].(map[string]any)
	tmpl, _ := asset["templateUrl"].(string)
	if tmpl == "" {
		return ""
	}
	w, _ := asset["width"].(float64)
	h, _ := asset["height"].(float64)
	format := "png"
	if ext == ".jpg" || ext == ".jpeg" {
		format = "jpg"
	}
	return strings.NewReplacer("{w}", fmt.Sprint(int(w)), "{h}", fmt.Sprint(int(h)), "{f}", format, "{c}", "bb").Replace(tmpl)
}

type pullResult struct {
	Written, Removed []string
	Notes            []string
}

// pullMedia writes App Store Connect's screenshots and previews for the version into
// metadata/screenshots and metadata/previews. A set that already matches is left alone.
func pullMedia(c *Client, cfg Config, r *Remote, kinds map[string]bool, only map[string]bool) (*pullResult, error) {
	res := &pullResult{}
	if r.Version == nil {
		res.Notes = append(res.Notes, "the app has no App Store version yet")
		return res, nil
	}
	for _, k := range []mediaKind{screenshotsKind, previewsKind} {
		if !kinds[k.name] {
			continue
		}
		for _, loc := range sortedKeys(r.VersionLocs) {
			if len(only) > 0 && !only[loc] {
				continue
			}
			sets, err := c.List("/v1/appStoreVersionLocalizations/"+r.VersionLocs[loc].ID+"/"+k.setType, nil)
			if err != nil {
				return nil, err
			}
			for _, set := range sets {
				items, err := c.List("/v1/"+k.setType+"/"+set.ID+"/"+k.itemType, nil)
				if err != nil {
					return nil, err
				}
				if len(items) == 0 {
					continue
				}
				dir := filepath.Join(cfg.MetadataPath(), k.name, loc, set.Str(k.typeAttr))
				if err := pullSet(c, cfg, k, dir, items, res); err != nil {
					return nil, err
				}
			}
		}
	}
	return res, nil
}

func pullSet(c *Client, cfg Config, k mediaKind, dir string, items []*Resource, res *pullResult) error {
	exts := imageExtensions
	defaultExt := ".png"
	if k.name == "previews" {
		exts, defaultExt = videoExtensions, ".mov"
	}
	var existing []string
	for _, f := range sortedDir(dir) {
		if !f.IsDir() && exts[strings.ToLower(filepath.Ext(f.Name()))] {
			existing = append(existing, filepath.Join(dir, f.Name()))
		}
	}
	same := len(existing) == len(items)
	for i := 0; same && i < len(items); i++ {
		sum, err := sourceChecksum(existing[i])
		same = err == nil && sum == items[i].Str("sourceFileChecksum")
	}
	if same {
		return nil
	}

	type download struct {
		name string
		data []byte
		src  string
	}
	var downloads []download
	for i, it := range items {
		name := pulledName(i, it.Str("fileName"), defaultExt, exts)
		var u string
		if k.name == "screenshots" {
			u = screenshotURL(it, strings.ToLower(filepath.Ext(name)))
		} else {
			u = it.Str("videoUrl")
		}
		where := cfg.Rel(filepath.Join(dir, name))
		if u == "" {
			res.Notes = append(res.Notes, where+": App Store Connect has no download for it yet (still processing?); set skipped")
			return nil
		}
		fmt.Fprintf(progress, "    downloading %s\n", where)
		data, ctype, err := c.Download(u)
		if err != nil {
			return err
		}
		if strings.Contains(ctype, "mpegurl") {
			res.Notes = append(res.Notes, where+": Apple only offers this preview as a stream; set skipped")
			return nil
		}
		downloads = append(downloads, download{name, data, it.Str("sourceFileChecksum")})
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	keep := map[string]bool{}
	for _, d := range downloads {
		keep[d.name] = true
	}
	for _, f := range existing {
		if !keep[filepath.Base(f)] {
			if err := os.Remove(f); err != nil {
				return err
			}
			res.Removed = append(res.Removed, f)
		}
	}
	sources := map[string]string{}
	for _, d := range downloads {
		path := filepath.Join(dir, d.name)
		if err := os.WriteFile(path, d.data, 0o644); err != nil {
			return err
		}
		res.Written = append(res.Written, path)
		if sum, err := fileMD5(path); err == nil && d.src != "" && sum != d.src {
			sources[sum] = d.src
		}
	}
	delete(sourcesCache, dir)
	sidecar := filepath.Join(dir, sourcesFile)
	if len(sources) == 0 {
		if err := os.Remove(sidecar); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	raw, _ := json.MarshalIndent(sources, "", "  ")
	return os.WriteFile(sidecar, append(raw, '\n'), 0o644)
}
