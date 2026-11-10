package main

import (
	"crypto/md5"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

var imageExtensions = setOf(".png", ".jpg", ".jpeg")
var videoExtensions = setOf(".mov", ".mp4", ".m4v")
var videoMIME = map[string]string{".mov": "video/quicktime", ".mp4": "video/mp4", ".m4v": "video/mp4"}

// Display types from Apple's OpenAPI spec (4.6). The iPhone Duo values aren't in the spec yet
// but are reported to work on the live API, so they only warn.
var screenshotDisplayTypes = setOf(
	"APP_IPHONE_67", "APP_IPHONE_61", "APP_IPHONE_65", "APP_IPHONE_58", "APP_IPHONE_55",
	"APP_IPHONE_47", "APP_IPHONE_40", "APP_IPHONE_35", "APP_IPAD_PRO_3GEN_129",
	"APP_IPAD_PRO_3GEN_11", "APP_IPAD_PRO_129", "APP_IPAD_105", "APP_IPAD_97", "APP_DESKTOP",
	"APP_WATCH_ULTRA", "APP_WATCH_SERIES_10", "APP_WATCH_SERIES_7", "APP_WATCH_SERIES_4",
	"APP_WATCH_SERIES_3", "APP_APPLE_TV", "APP_APPLE_VISION_PRO", "IMESSAGE_APP_IPHONE_67",
	"IMESSAGE_APP_IPHONE_61", "IMESSAGE_APP_IPHONE_65", "IMESSAGE_APP_IPHONE_58",
	"IMESSAGE_APP_IPHONE_55", "IMESSAGE_APP_IPHONE_47", "IMESSAGE_APP_IPHONE_40",
	"IMESSAGE_APP_IPAD_PRO_3GEN_129", "IMESSAGE_APP_IPAD_PRO_3GEN_11", "IMESSAGE_APP_IPAD_PRO_129",
	"IMESSAGE_APP_IPAD_105", "IMESSAGE_APP_IPAD_97",
)
var undocumentedDisplayTypes = setOf("APP_IPHONE_DUO", "IMESSAGE_APP_IPHONE_DUO")
var previewTypes = setOf(
	"IPHONE_67", "IPHONE_61", "IPHONE_65", "IPHONE_58", "IPHONE_55", "IPHONE_47", "IPHONE_40",
	"IPHONE_35", "IPAD_PRO_3GEN_129", "IPAD_PRO_3GEN_11", "IPAD_PRO_129", "IPAD_105", "IPAD_97",
	"DESKTOP", "APPLE_TV", "APPLE_VISION_PRO",
)
var undocumentedPreviewTypes = setOf("IPHONE_DUO")

const maxScreenshots = 10
const maxPreviews = 3

// creative_assets/<slot>.<ext> -> Asset Library placement type
var creativeSlots = map[string]string{
	"header":         "PRODUCT_PAGE_HEADER_ASSET",
	"search_results": "APP_STORE_SEARCH_RESULTS_ASSET",
}

const referencePrefix = "appbird-"

// progress receives upload progress lines; run() points it at the command's output.
var progress io.Writer = os.Stdout

func fileMD5(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := md5.New() // Apple's sourceFileChecksum is an MD5
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// pngInfo reads a PNG's size and whether it has an alpha channel.
func pngInfo(path string) (w, h int, alpha, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	head := make([]byte, 26)
	if _, err := io.ReadFull(f, head); err != nil {
		return
	}
	if string(head[:8]) != "\x89PNG\r\n\x1a\n" || string(head[12:16]) != "IHDR" {
		return
	}
	colorType := head[25]
	return int(binary.BigEndian.Uint32(head[16:20])), int(binary.BigEndian.Uint32(head[20:24])),
		colorType == 4 || colorType == 6, true
}

// LocalMedia is what's in screenshots/, previews/ and creative_assets/.
type LocalMedia struct {
	Screenshots  map[string]map[string][]string // locale -> display type -> files in order
	Previews     map[string]map[string][]string
	Creative     map[string]map[string]string // locale ("*" = every locale) -> slot -> file
	UnknownFiles []string
}

func (m *LocalMedia) counts() (shots, previews, creative int) {
	for _, s := range m.Screenshots {
		for _, f := range s {
			shots += len(f)
		}
	}
	for _, s := range m.Previews {
		for _, f := range s {
			previews += len(f)
		}
	}
	for _, s := range m.Creative {
		creative += len(s)
	}
	return
}

func loadMedia(cfg Config) *LocalMedia {
	root := cfg.MetadataPath()
	m := &LocalMedia{Screenshots: map[string]map[string][]string{}, Previews: map[string]map[string][]string{},
		Creative: map[string]map[string]string{}}
	for _, kind := range []struct {
		dir    string
		target map[string]map[string][]string
		exts   map[string]bool
	}{{"screenshots", m.Screenshots, imageExtensions}, {"previews", m.Previews, videoExtensions}} {
		base := filepath.Join(root, kind.dir)
		for _, loc := range sortedDir(base) {
			if !loc.IsDir() {
				continue
			}
			for _, set := range sortedDir(filepath.Join(base, loc.Name())) {
				if !set.IsDir() {
					continue
				}
				dir := filepath.Join(base, loc.Name(), set.Name())
				var files []string
				for _, f := range sortedDir(dir) {
					name := f.Name()
					if strings.HasPrefix(name, ".") {
						continue
					}
					if !f.IsDir() && kind.exts[strings.ToLower(filepath.Ext(name))] {
						files = append(files, filepath.Join(dir, name))
					} else {
						m.UnknownFiles = append(m.UnknownFiles, filepath.Join(dir, name))
					}
				}
				if len(files) > 0 {
					if kind.target[loc.Name()] == nil {
						kind.target[loc.Name()] = map[string][]string{}
					}
					kind.target[loc.Name()][set.Name()] = files
				}
			}
		}
	}
	base := filepath.Join(root, "creative_assets")
	slots := func(dir string) map[string]string {
		found := map[string]string{}
		for _, f := range sortedDir(dir) {
			name := f.Name()
			if f.IsDir() || strings.HasPrefix(name, ".") || name == "README.md" {
				continue
			}
			ext := strings.ToLower(filepath.Ext(name))
			stem := strings.TrimSuffix(name, filepath.Ext(name))
			if _, ok := creativeSlots[stem]; ok && (imageExtensions[ext] || videoExtensions[ext]) {
				found[stem] = filepath.Join(dir, name)
			} else {
				m.UnknownFiles = append(m.UnknownFiles, filepath.Join(dir, name))
			}
		}
		return found
	}
	if d := slots(base); len(d) > 0 {
		m.Creative["*"] = d
	}
	for _, loc := range sortedDir(base) {
		if loc.IsDir() {
			if d := slots(filepath.Join(base, loc.Name())); len(d) > 0 {
				m.Creative[loc.Name()] = d
			}
		}
	}
	return m
}

func validateMedia(cfg Config, m *LocalMedia) (problems, warnings []string) {
	for _, p := range m.UnknownFiles {
		warnings = append(warnings, "ignored (not a media file appbird knows): "+cfg.Rel(p))
	}
	for _, loc := range sortedKeys(m.Screenshots) {
		for _, t := range sortedKeys(m.Screenshots[loc]) {
			where := "screenshots/" + loc + "/" + t
			switch {
			case undocumentedDisplayTypes[t]:
				warnings = append(warnings, where+": not in Apple's published API yet; the live API is reported to accept it")
			case !screenshotDisplayTypes[t]:
				warnings = append(warnings, where+": not a display type Apple documents")
			}
			if n := len(m.Screenshots[loc][t]); n > maxScreenshots {
				problems = append(problems, fmt.Sprintf("%s: %d files, the limit is %d", where, n, maxScreenshots))
			}
		}
	}
	for _, loc := range sortedKeys(m.Previews) {
		for _, t := range sortedKeys(m.Previews[loc]) {
			where := "previews/" + loc + "/" + t
			switch {
			case undocumentedPreviewTypes[t]:
				warnings = append(warnings, where+": not in Apple's published API yet")
			case !previewTypes[t]:
				warnings = append(warnings, where+": not a preview type Apple documents")
			}
			if n := len(m.Previews[loc][t]); n > maxPreviews {
				problems = append(problems, fmt.Sprintf("%s: %d files, the limit is %d", where, n, maxPreviews))
			}
		}
	}
	for _, loc := range sortedKeys(m.Creative) {
		for _, slot := range sortedKeys(m.Creative[loc]) {
			path := m.Creative[loc][slot]
			w, h, alpha, ok := pngInfo(path)
			if !ok {
				continue
			}
			if alpha {
				problems = append(problems, cfg.Rel(path)+": has an alpha channel; Apple wants creative assets without one")
			}
			// Apple's creative asset specifications.
			good := w == 5244 && h == 2950
			if slot == "header" {
				good = good || (w == 3840 && h == 1646)
			} else {
				good = good || (w*2 == h*3 && w >= 1920 && w <= 3840)
			}
			if !good {
				warnings = append(warnings, fmt.Sprintf("%s: %dx%d isn't one of Apple's sizes for %s", cfg.Rel(path), w, h, slot))
			}
		}
	}
	return problems, warnings
}

type remoteSet struct {
	Set   *Resource
	Items []*Resource
}

type remotePlacement struct {
	Placement *Resource
	Asset     *Resource // the placed image or video, if included
}

// RemoteMedia is App Store Connect's media for the chosen version.
type RemoteMedia struct {
	ScreenshotSets map[string]map[string]*remoteSet // locale -> display type
	PreviewSets    map[string]map[string]*remoteSet
	Placements     map[string]map[string]*remotePlacement // locale -> placement type
	LibraryID      string
	LibraryAssets  map[string]*Resource // reference name -> image or video
}

func fetchMedia(c *Client, r *Remote, m *LocalMedia) (*RemoteMedia, error) {
	out := &RemoteMedia{ScreenshotSets: map[string]map[string]*remoteSet{}, PreviewSets: map[string]map[string]*remoteSet{},
		Placements: map[string]map[string]*remotePlacement{}, LibraryAssets: map[string]*Resource{}}
	wanted := map[string]bool{}
	for l := range m.Screenshots {
		wanted[l] = true
	}
	for l := range m.Previews {
		wanted[l] = true
	}
	for l := range m.Creative {
		if l == "*" {
			for rl := range r.VersionLocs {
				wanted[rl] = true
			}
		} else {
			wanted[l] = true
		}
	}
	for _, loc := range sortedKeys(wanted) {
		res := r.VersionLocs[loc]
		if res == nil {
			continue
		}
		if _, ok := m.Screenshots[loc]; ok {
			sets, err := c.List("/v1/appStoreVersionLocalizations/"+res.ID+"/appScreenshotSets", nil)
			if err != nil {
				return nil, err
			}
			out.ScreenshotSets[loc] = map[string]*remoteSet{}
			for _, s := range sets {
				items, err := c.List("/v1/appScreenshotSets/"+s.ID+"/appScreenshots", nil)
				if err != nil {
					return nil, err
				}
				out.ScreenshotSets[loc][s.Str("screenshotDisplayType")] = &remoteSet{s, items}
			}
		}
		if _, ok := m.Previews[loc]; ok {
			sets, err := c.List("/v1/appStoreVersionLocalizations/"+res.ID+"/appPreviewSets", nil)
			if err != nil {
				return nil, err
			}
			out.PreviewSets[loc] = map[string]*remoteSet{}
			for _, s := range sets {
				items, err := c.List("/v1/appPreviewSets/"+s.ID+"/appPreviews", nil)
				if err != nil {
					return nil, err
				}
				out.PreviewSets[loc][s.Str("previewType")] = &remoteSet{s, items}
			}
		}
		if len(m.Creative) > 0 {
			types := make([]string, 0, len(creativeSlots))
			for _, t := range creativeSlots {
				types = append(types, t)
			}
			sort.Strings(types)
			placements, included, err := c.GetAll("/v1/appStoreVersionLocalizations/"+res.ID+"/placements",
				url.Values{"filter[placementType]": {strings.Join(types, ",")}, "include": {"image,video"}})
			if err != nil {
				return nil, err
			}
			out.Placements[loc] = map[string]*remotePlacement{}
			for _, p := range placements {
				rp := &remotePlacement{Placement: p}
				for _, relName := range []string{"image", "video"} {
					if t, id := p.Related(relName); id != "" {
						rp.Asset = included[t+"/"+id]
					}
				}
				out.Placements[loc][p.Str("placementType")] = rp
			}
		}
	}
	if len(m.Creative) > 0 {
		lib, err := c.GetOne("/v1/apps/" + r.App.ID + "/assetLibrary")
		if err != nil {
			return nil, err
		}
		if lib != nil {
			out.LibraryID = lib.ID
			for _, kind := range []string{"images", "videos"} {
				assets, err := c.List("/v1/appAssetLibraries/"+lib.ID+"/"+kind, url.Values{"filter[category]": {"CREATIVE_ASSETS"}})
				if err != nil {
					return nil, err
				}
				for _, a := range assets {
					ref, state := a.Str("referenceName"), a.Str("state")
					if strings.HasPrefix(ref, referencePrefix) && state != "FAILED" && state != "ARCHIVED" {
						out.LibraryAssets[ref] = a
					}
				}
			}
		}
	}
	return out, nil
}

// waitFor polls a just-uploaded asset until Apple has processed it.
func waitFor(c *Client, rtype, id, label string, stateOf func(*Resource) string) error {
	deadline := time.Now().Add(10 * time.Minute)
	delay := 500 * time.Millisecond
	for {
		res, err := c.GetOne("/v1/" + rtype + "/" + id)
		if err != nil {
			return err
		}
		state := stateOf(res)
		switch state {
		case "FAILED":
			return userErr("App Store Connect couldn't process %s", label)
		case "AWAITING_UPLOAD", "UPLOAD_COMPLETE", "PROCESSING", "":
		default:
			return nil
		}
		if time.Now().After(deadline) {
			fmt.Fprintf(os.Stderr, "  still processing %s; check App Store Connect\n", label)
			return nil
		}
		time.Sleep(delay)
		if delay < 10*time.Second {
			delay *= 2
		}
	}
}

func nestedState(res *Resource, keys ...string) string {
	if res == nil {
		return ""
	}
	for _, k := range keys {
		if m, ok := res.Attributes[k].(map[string]any); ok {
			if s, ok := m["state"].(string); ok {
				return s
			}
		}
	}
	return ""
}

type mediaKind struct {
	name, setType, itemType, setRel, typeAttr string
}

var screenshotsKind = mediaKind{"screenshots", "appScreenshotSets", "appScreenshots", "appScreenshotSet", "screenshotDisplayType"}
var previewsKind = mediaKind{"previews", "appPreviewSets", "appPreviews", "appPreviewSet", "previewType"}

func uploadSetItem(c *Client, k mediaKind, setID, path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum, _ := fileMD5(path)
	name := filepath.Base(path)
	attrs := map[string]any{"fileName": name, "fileSize": len(data)}
	if k.name == "previews" {
		mime := videoMIME[strings.ToLower(filepath.Ext(path))]
		if mime == "" {
			mime = "video/quicktime"
		}
		attrs["mimeType"] = mime
	}
	res, err := c.Create(k.itemType, attrs, map[string]any{k.setRel: rel(k.setType, setID)})
	if err != nil {
		return "", err
	}
	if err := c.UploadParts(res, data); err != nil {
		return "", err
	}
	if err := c.Patch(k.itemType, res.ID, map[string]any{"uploaded": true, "sourceFileChecksum": sum}); err != nil {
		return "", err
	}
	return res.ID, waitFor(c, k.itemType, res.ID, name, func(r *Resource) string {
		return nestedState(r, "assetDeliveryState", "videoDeliveryState")
	})
}

func setOperation(k mediaKind, loc, typ string, files []string, remote *remoteSet, locID string) (*Operation, error) {
	type localFile struct{ path, sum string }
	var local []localFile
	localSums := map[string]bool{}
	for _, f := range files {
		sum, err := sourceChecksum(f)
		if err != nil {
			return nil, err
		}
		local = append(local, localFile{f, sum})
		localSums[sum] = true
	}
	var items []*Resource
	if remote != nil {
		items = remote.Items
	}
	same := remote != nil && len(items) == len(local)
	for i := 0; same && i < len(items); i++ {
		same = items[i].Str("sourceFileChecksum") == local[i].sum
	}
	if same {
		return nil, nil
	}
	keep := map[string]*Resource{}
	var toDelete []*Resource
	for _, it := range items {
		if sum := it.Str("sourceFileChecksum"); localSums[sum] && keep[sum] == nil {
			keep[sum] = it
		} else {
			toDelete = append(toDelete, it)
		}
	}
	var toUpload []localFile
	var oldNames, newNames []string
	for _, it := range items {
		oldNames = append(oldNames, it.Str("fileName"))
	}
	for _, f := range local {
		label := filepath.Base(f.path)
		if keep[f.sum] == nil {
			toUpload = append(toUpload, f)
			label += "  (new)"
		}
		newNames = append(newNames, label)
	}
	change := &Change{Label: k.name + "/" + loc + "/" + typ, Old: strings.Join(oldNames, "\n"), New: strings.Join(newNames, "\n")}

	run := func(c *Client) error {
		var setID string
		if remote != nil {
			setID = remote.Set.ID
		} else {
			set, err := c.Create(k.setType, map[string]any{k.typeAttr: typ},
				map[string]any{"appStoreVersionLocalization": rel("appStoreVersionLocalizations", locID)})
			if err != nil {
				return err
			}
			setID = set.ID
		}
		ids := map[string]string{}
		for sum, it := range keep {
			ids[sum] = it.ID
		}
		for _, f := range toUpload {
			fmt.Fprintf(progress, "    uploading %s\n", filepath.Base(f.path))
			id, err := uploadSetItem(c, k, setID, f.path)
			if err != nil {
				return err
			}
			ids[f.sum] = id
		}
		for _, it := range toDelete {
			fmt.Fprintf(progress, "    removing %s\n", it.Str("fileName"))
			if err := c.Delete(k.itemType, it.ID); err != nil {
				return err
			}
		}
		order := make([]map[string]string, 0, len(local))
		for _, f := range local {
			order = append(order, map[string]string{"type": k.itemType, "id": ids[f.sum]})
		}
		_, err := c.do("PATCH", "/v1/"+k.setType+"/"+setID+"/relationships/"+k.itemType, nil, map[string]any{"data": order})
		return err
	}
	verb := "add"
	if remote != nil {
		verb = "replace"
	}
	return &Operation{fmt.Sprintf("%s %s %s %s (%d upload, %d remove)", verb, k.name, loc, typ, len(toUpload), len(toDelete)),
		run, []*Change{change}}, nil
}

func buildMediaPlan(m *LocalMedia, r *Remote, rm *RemoteMedia) (*Plan, error) {
	p := &Plan{}
	lock := ""
	switch {
	case r.Version == nil:
		lock = "the app has no App Store version yet"
	case !r.VersionEditable:
		lock = fmt.Sprintf("version %s is %s; screenshots and previews change only on a version being prepared",
			r.VersionString(), versionState(r.Version))
	}
	for _, kind := range []struct {
		k      mediaKind
		local  map[string]map[string][]string
		remote map[string]map[string]*remoteSet
	}{{screenshotsKind, m.Screenshots, rm.ScreenshotSets}, {previewsKind, m.Previews, rm.PreviewSets}} {
		for _, loc := range sortedKeys(kind.local) {
			res := r.VersionLocs[loc]
			if res == nil {
				p.Notes = append(p.Notes, fmt.Sprintf("%s/%s: the version has no %s localization; push the text first", kind.k.name, loc, loc))
				continue
			}
			for _, typ := range sortedKeys(kind.local[loc]) {
				op, err := setOperation(kind.k, loc, typ, kind.local[loc][typ], kind.remote[loc][typ], res.ID)
				if err != nil {
					return nil, err
				}
				if op == nil {
					continue
				}
				if lock != "" {
					p.lock(lock, op.Changes...)
				} else {
					p.Operations = append(p.Operations, op)
				}
			}
		}
	}

	if len(m.Creative) == 0 {
		return p, nil
	}
	if rm.LibraryID == "" {
		p.Notes = append(p.Notes, "this app has no Asset Library in App Store Connect yet; creative assets skipped")
		return p, nil
	}
	uploaded := map[string][2]string{} // reference name -> (type, id), shared across locales
	ensureAsset := func(c *Client, path, ref string) (string, string, error) {
		rtype := "appAssetLibraryImages"
		if videoExtensions[strings.ToLower(filepath.Ext(path))] {
			rtype = "appAssetLibraryVideos"
		}
		if u, ok := uploaded[ref]; ok {
			return u[0], u[1], nil
		}
		if a := rm.LibraryAssets[ref]; a != nil {
			uploaded[ref] = [2]string{a.Type, a.ID}
			return a.Type, a.ID, nil
		}
		fmt.Fprintf(progress, "    uploading %s to the Asset Library\n", filepath.Base(path))
		data, err := os.ReadFile(path)
		if err != nil {
			return "", "", err
		}
		res, err := c.Create(rtype, map[string]any{"category": "CREATIVE_ASSETS", "fileName": filepath.Base(path),
			"fileSize": len(data), "referenceName": ref},
			map[string]any{"assetLibrary": rel("appAssetLibraries", rm.LibraryID)})
		if err != nil {
			return "", "", err
		}
		if err := c.UploadParts(res, data); err != nil {
			return "", "", err
		}
		if err := c.Patch(rtype, res.ID, map[string]any{"uploaded": true}); err != nil {
			return "", "", err
		}
		if err := waitFor(c, rtype, res.ID, filepath.Base(path), func(r *Resource) string { return r.Str("state") }); err != nil {
			return "", "", err
		}
		uploaded[ref] = [2]string{rtype, res.ID}
		return rtype, res.ID, nil
	}

	locales := map[string]bool{}
	if _, ok := m.Creative["*"]; ok {
		for l := range r.VersionLocs {
			locales[l] = true
		}
	}
	for l := range m.Creative {
		if l != "*" {
			locales[l] = true
		}
	}
	placed := false
	for _, loc := range sortedKeys(locales) {
		res := r.VersionLocs[loc]
		if res == nil {
			p.Notes = append(p.Notes, "creative_assets/"+loc+": the version has no "+loc+" localization; skipped")
			continue
		}
		slots := map[string]string{}
		for s, f := range m.Creative["*"] {
			slots[s] = f
		}
		for s, f := range m.Creative[loc] {
			slots[s] = f
		}
		for _, slot := range sortedKeys(slots) {
			path, ptype := slots[slot], creativeSlots[slot]
			sum, err := fileMD5(path)
			if err != nil {
				return nil, err
			}
			ref := referencePrefix + sum
			current := rm.Placements[loc][ptype]
			var currentAsset *Resource
			if current != nil {
				currentAsset = current.Asset
			}
			if current != nil && currentAsset.Str("referenceName") == ref {
				continue
			}
			old := currentAsset.Str("fileName")
			if current != nil && old == "" {
				old = "(something placed outside appbird)"
			}
			change := &Change{Label: "creative_assets/" + loc + "/" + slot, Old: old, New: filepath.Base(path)}
			locID := res.ID
			p.Operations = append(p.Operations, &Operation{"place " + slot + " on " + loc, func(c *Client) error {
				rtype, id, err := ensureAsset(c, path, ref)
				if err != nil {
					return err
				}
				if current != nil {
					// A placement can't be edited: remove the old one, then place the new asset.
					if err := c.Delete("appAssetLibraryPlacements", current.Placement.ID); err != nil {
						return err
					}
				}
				relName := "image"
				if rtype == "appAssetLibraryVideos" {
					relName = "video"
				}
				_, err = c.Create("appAssetLibraryPlacements", map[string]any{"placementType": ptype}, map[string]any{
					"appStoreVersionLocalization": rel("appStoreVersionLocalizations", locID),
					relName:                       rel(rtype, id)})
				return err
			}, []*Change{change}})
			placed = true
		}
	}
	if placed {
		p.Notes = append(p.Notes, "new creative assets need Apple's approval: submit them from App Store Connect › Asset Library (appbird never submits anything)")
	}
	return p, nil
}
