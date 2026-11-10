package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

const configName = "appbird.json"

var platforms = setOf("IOS", "MAC_OS", "TV_OS", "VISION_OS")

// configFor names the config file and metadata folder `appbird init` writes for a platform.
// iOS keeps the plain names; other platforms get their own, so one repo can hold several.
func configFor(platform string) (file, metadataDir string) {
	if platform == "IOS" {
		return configName, "metadata"
	}
	short := strings.ToLower(strings.ReplaceAll(platform, "_", ""))
	return "appbird." + short + ".json", "metadata-" + short
}

// configIn finds the config in one folder: appbird.json, or else the only appbird.<platform>.json.
func configIn(dir string) (string, error) {
	candidate := filepath.Join(dir, configName)
	if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
		return candidate, nil
	}
	var found []string
	for p := range platforms {
		name, _ := configFor(p)
		if info, err := os.Stat(filepath.Join(dir, name)); err == nil && !info.IsDir() && name != configName {
			found = append(found, name)
		}
	}
	sort.Strings(found)
	switch len(found) {
	case 0:
		return "", nil
	case 1:
		return filepath.Join(dir, found[0]), nil
	}
	return "", userErr("Found %s in %s. Pick one with -c.", strings.Join(found, " and "), dir)
}

// Config is an app repo's appbird.json.
type Config struct {
	Root        string `json:"-"`
	BundleID    string `json:"bundle_id"`
	Platform    string `json:"platform"`
	MetadataDir string `json:"metadata_dir"`
}

func (c Config) MetadataPath() string { return filepath.Join(c.Root, c.MetadataDir) }

func (c Config) Rel(path string) string {
	if r, err := filepath.Rel(c.Root, path); err == nil {
		return r
	}
	return path
}

func findConfig(start, explicit string) (Config, error) {
	path := explicit
	if path == "" {
		dir, _ := filepath.Abs(start)
		for {
			found, err := configIn(dir)
			if err != nil {
				return Config{}, err
			}
			if found != "" {
				path = found
				break
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				return Config{}, userErr("No %s found here or in a parent folder. Run `appbird init`.", configName)
			}
			dir = parent
		}
	}
	path, _ = filepath.Abs(path)
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, userErr("Can't read %s: %v", path, err)
	}
	cfg := Config{Platform: "IOS", MetadataDir: "metadata"}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return Config{}, userErr("Can't read %s: %v", path, err)
	}
	if cfg.BundleID == "" {
		return Config{}, userErr("%s needs a \"bundle_id\".", path)
	}
	cfg.Root = filepath.Dir(path)
	return cfg, nil
}

// normalize compares text the way App Store Connect stores it: no CRs, no trailing whitespace.
func normalize(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return strings.TrimRightFunc(s, unicode.IsSpace)
}

// readText returns (value, true) for an existing file. A missing file isn't managed; an empty
// file means "clear the field".
func readText(path string) (string, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	return normalize(string(raw)), true
}

func writeText(path, value string) (bool, error) {
	text := normalize(value)
	if text != "" {
		text += "\n"
	}
	if old, err := os.ReadFile(path); err == nil && string(old) == text {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	return true, os.WriteFile(path, []byte(text), 0o644)
}

// optional is a file that may or may not exist.
type optional struct {
	Value string
	Set   bool
}

type localSubscription struct {
	ReviewNote optional
	Locales    map[string]map[string]string
}

// LocalMetadata is everything read from metadata/.
type LocalMetadata struct {
	Copyright          optional
	EULA               optional
	ReviewNotes        optional
	AppInfo            map[string]map[string]string // locale -> file -> value
	Version            map[string]map[string]string
	Subscriptions      map[string]*localSubscription           // product id
	InAppPurchases     map[string]*localSubscription           // product id
	SubscriptionGroups map[string]map[string]map[string]string // reference name -> locale -> file
	UnknownFiles       []string
}

func readOptional(path string) optional {
	v, ok := readText(path)
	return optional{v, ok}
}

func sortedDir(path string) []os.DirEntry {
	entries, _ := os.ReadDir(path)
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	return entries
}

func readLocaleDir(dir string, fields []field, unknown *[]string) map[string]string {
	known := fieldNames(fields)
	values := map[string]string{}
	for _, e := range sortedDir(dir) {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		stem := strings.TrimSuffix(name, ".txt")
		if !e.IsDir() && strings.HasSuffix(name, ".txt") && known[stem] {
			v, _ := readText(filepath.Join(dir, name))
			values[stem] = v
		} else {
			*unknown = append(*unknown, filepath.Join(dir, name))
		}
	}
	return values
}

// readProducts reads subscriptions/ or in_app_purchases/: a folder per product ID holding
// review_note.txt and a folder per locale.
func readProducts(path string, fields []field, into map[string]*localSubscription, unknown *[]string) {
	for _, p := range sortedDir(path) {
		if !p.IsDir() {
			continue
		}
		dir := filepath.Join(path, p.Name())
		prod := &localSubscription{ReviewNote: readOptional(filepath.Join(dir, "review_note.txt")),
			Locales: map[string]map[string]string{}}
		for _, l := range sortedDir(dir) {
			if l.IsDir() {
				prod.Locales[l.Name()] = readLocaleDir(filepath.Join(dir, l.Name()), fields, unknown)
			}
		}
		into[p.Name()] = prod
	}
}

func loadLocal(cfg Config) (*LocalMetadata, error) {
	root := cfg.MetadataPath()
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		return nil, userErr("%s doesn't exist. Run `appbird pull` to create it from App Store Connect.", root)
	}
	m := &LocalMetadata{
		AppInfo: map[string]map[string]string{}, Version: map[string]map[string]string{},
		Subscriptions: map[string]*localSubscription{}, SubscriptionGroups: map[string]map[string]map[string]string{},
		InAppPurchases: map[string]*localSubscription{},
	}
	m.Copyright = readOptional(filepath.Join(root, "copyright.txt"))
	m.EULA = readOptional(filepath.Join(root, "eula.txt"))
	m.ReviewNotes = readOptional(filepath.Join(root, "review_information", "notes.txt"))
	allLocaleFields := append(append([]field{}, appInfoFields...), versionFields...)
	appInfoNames, versionNames := fieldNames(appInfoFields), fieldNames(versionFields)

	for _, e := range sortedDir(root) {
		name := e.Name()
		path := filepath.Join(root, name)
		switch {
		case strings.HasPrefix(name, "."), name == "copyright.txt", name == "eula.txt", name == "README.md":
		case e.IsDir() && name == "review_information":
			for _, f := range sortedDir(path) {
				if f.Name() != "notes.txt" && !strings.HasPrefix(f.Name(), ".") {
					m.UnknownFiles = append(m.UnknownFiles, filepath.Join(path, f.Name()))
				}
			}
		case e.IsDir() && name == "subscriptions":
			readProducts(path, subscriptionLocFields, m.Subscriptions, &m.UnknownFiles)
		case e.IsDir() && name == "in_app_purchases":
			readProducts(path, inAppPurchaseLocFields, m.InAppPurchases, &m.UnknownFiles)
		case e.IsDir() && name == "subscription_groups":
			for _, g := range sortedDir(path) {
				if !g.IsDir() {
					continue
				}
				dir := filepath.Join(path, g.Name())
				locs := map[string]map[string]string{}
				for _, l := range sortedDir(dir) {
					if l.IsDir() {
						locs[l.Name()] = readLocaleDir(filepath.Join(dir, l.Name()), subscriptionGroupLocFields, &m.UnknownFiles)
					}
				}
				m.SubscriptionGroups[g.Name()] = locs
			}
		case e.IsDir() && specialDirs[name]:
		case e.IsDir():
			values := readLocaleDir(path, allLocaleFields, &m.UnknownFiles)
			info, ver := map[string]string{}, map[string]string{}
			for k, v := range values {
				if appInfoNames[k] {
					info[k] = v
				} else if versionNames[k] {
					ver[k] = v
				}
			}
			m.AppInfo[name], m.Version[name] = info, ver
		default:
			m.UnknownFiles = append(m.UnknownFiles, path)
		}
	}
	return m, nil
}

var urlRE = regexp.MustCompile(`^https?://\S+$`)

func checkValue(problems, warnings *[]string, where, name, value string, limit int) {
	if n := utf8.RuneCountInString(value); limit > 0 && n > limit {
		*problems = append(*problems, fmt.Sprintf("%s/%s: %d characters, the limit is %d", where, name, n, limit))
	}
	if urlFields[name] && value != "" && !urlRE.MatchString(value) {
		*problems = append(*problems, fmt.Sprintf("%s/%s: not a URL: %q", where, name, value))
	}
	if name == "keywords" && value != "" {
		if len(value) > 100 {
			*warnings = append(*warnings, fmt.Sprintf("%s/keywords: %d bytes in UTF-8; App Store Connect may count bytes, so keep it to 100", where, len(value)))
		}
		if strings.Contains(value, ", ") {
			*warnings = append(*warnings, fmt.Sprintf("%s/keywords: spaces after commas use up the 100-character limit", where))
		}
	}
	if oneLineFields[name] && strings.Contains(value, "\n") {
		*problems = append(*problems, fmt.Sprintf("%s/%s: must be one line", where, name))
	}
}

func limitOf(fields []field, name string) int {
	for _, f := range fields {
		if f.File == name {
			return f.Limit
		}
	}
	return 0
}

func validateLocal(cfg Config, m *LocalMetadata) (problems, warnings []string) {
	for _, p := range m.UnknownFiles {
		warnings = append(warnings, "ignored (not a field appbird knows): "+cfg.Rel(p))
	}
	locales := map[string]bool{}
	for l := range m.AppInfo {
		locales[l] = true
	}
	for _, products := range []map[string]*localSubscription{m.Subscriptions, m.InAppPurchases} {
		for _, s := range products {
			for l := range s.Locales {
				locales[l] = true
			}
		}
	}
	for _, g := range m.SubscriptionGroups {
		for l := range g {
			locales[l] = true
		}
	}
	for _, l := range sortedKeys(locales) {
		if !localeRE.MatchString(l) {
			problems = append(problems, l+": not a locale code")
		} else if !knownLocales[l] {
			warnings = append(warnings, l+": not a listing locale App Store Connect is known to use (Latin American Spanish is es-MX, Spain is es-ES)")
		}
	}
	for _, l := range sortedKeys(m.AppInfo) {
		for _, k := range sortedKeys(m.AppInfo[l]) {
			checkValue(&problems, &warnings, l, k, m.AppInfo[l][k], limitOf(appInfoFields, k))
		}
		for _, k := range sortedKeys(m.Version[l]) {
			checkValue(&problems, &warnings, l, k, m.Version[l][k], limitOf(versionFields, k))
		}
	}
	if n := utf8.RuneCountInString(m.ReviewNotes.Value); n > reviewNotesLimit {
		problems = append(problems, fmt.Sprintf("review_information/notes: %d characters, the limit is %d", n, reviewNotesLimit))
	}
	for _, kind := range []struct {
		dir      string
		products map[string]*localSubscription
		fields   []field
		limit    int
	}{
		{"subscriptions", m.Subscriptions, subscriptionLocFields, subscriptionReviewNoteLimit},
		{"in_app_purchases", m.InAppPurchases, inAppPurchaseLocFields, inAppPurchaseReviewNoteLimit},
	} {
		for _, pid := range sortedKeys(kind.products) {
			s := kind.products[pid]
			if n := utf8.RuneCountInString(s.ReviewNote.Value); n > kind.limit {
				problems = append(problems, fmt.Sprintf("%s/%s/review_note: %d characters, the limit is %d", kind.dir, pid, n, kind.limit))
			}
			for _, l := range sortedKeys(s.Locales) {
				for _, k := range sortedKeys(s.Locales[l]) {
					checkValue(&problems, &warnings, kind.dir+"/"+pid+"/"+l, k, s.Locales[l][k], limitOf(kind.fields, k))
				}
			}
		}
	}
	for _, g := range sortedKeys(m.SubscriptionGroups) {
		for _, l := range sortedKeys(m.SubscriptionGroups[g]) {
			for k, v := range m.SubscriptionGroups[g][l] {
				checkValue(&problems, &warnings, "subscription_groups/"+g+"/"+l, k, v, limitOf(subscriptionGroupLocFields, k))
			}
		}
	}
	return problems, warnings
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
