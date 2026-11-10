package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
)

// metadata/app_info.json holds the App Info settings that aren't per-locale text: categories,
// the age rating questionnaire, and the content rights declaration. Each key is optional; a
// missing key isn't managed.
const appInfoFile = "app_info.json"

// App Info's category relationships, in the order they're written.
var categoryKeys = []struct{ File, Rel string }{
	{"primary_category", "primaryCategory"},
	{"primary_subcategory_one", "primarySubcategoryOne"},
	{"primary_subcategory_two", "primarySubcategoryTwo"},
	{"secondary_category", "secondaryCategory"},
	{"secondary_subcategory_one", "secondarySubcategoryOne"},
	{"secondary_subcategory_two", "secondarySubcategoryTwo"},
}

var contentRightsValues = setOf("DOES_NOT_USE_THIRD_PARTY_CONTENT", "USES_THIRD_PARTY_CONTENT")

var categoryIDRE = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// localAppInfo is app_info.json. Category values are "" for "no category".
type localAppInfo struct {
	Path          string
	Categories    map[string]string // file key -> category ID
	ContentRights optional
	AgeRating     map[string]any // attribute -> value, passed to Apple as is
	Problems      []string
}

func readAppInfo(root string) *localAppInfo {
	path := filepath.Join(root, appInfoFile)
	raw, ok := readText(path)
	if !ok {
		return nil
	}
	info := &localAppInfo{Path: path, Categories: map[string]string{}}
	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &top); err != nil {
		info.Problems = append(info.Problems, fmt.Sprintf("%s: not valid JSON: %v", appInfoFile, err))
		return info
	}
	known := map[string]bool{"content_rights": true, "age_rating": true}
	for _, k := range categoryKeys {
		known[k.File] = true
	}
	for _, key := range sortedKeys(top) {
		value := top[key]
		where := appInfoFile + ": " + key
		switch {
		case !known[key]:
			info.Problems = append(info.Problems, where+": not a key appbird knows (categories, content_rights, age_rating)")
		case key == "age_rating":
			var m map[string]any
			if err := json.Unmarshal(value, &m); err != nil || m == nil {
				info.Problems = append(info.Problems, where+": must be an object of Apple's age rating attributes")
				continue
			}
			for _, attr := range sortedKeys(m) {
				switch m[attr].(type) {
				case string, bool, nil:
				default:
					info.Problems = append(info.Problems, fmt.Sprintf("%s.%s: must be a string, true/false or null", where, attr))
				}
			}
			info.AgeRating = m
		default:
			var s *string
			if err := json.Unmarshal(value, &s); err != nil {
				info.Problems = append(info.Problems, where+": must be a string or null")
				continue
			}
			v := ""
			if s != nil {
				v = strings.TrimSpace(*s)
			}
			if key == "content_rights" {
				if v != "" && !contentRightsValues[v] {
					info.Problems = append(info.Problems, where+": must be DOES_NOT_USE_THIRD_PARTY_CONTENT or USES_THIRD_PARTY_CONTENT")
				}
				info.ContentRights = optional{v, true}
				continue
			}
			if v != "" && !categoryIDRE.MatchString(v) {
				info.Problems = append(info.Problems, fmt.Sprintf("%s: %q isn't an App Store category ID (like PRODUCTIVITY or GAMES_PUZZLE)", where, v))
			}
			if key == "primary_category" && v == "" {
				info.Problems = append(info.Problems, where+": the primary category can't be empty")
			}
			info.Categories[key] = v
		}
	}
	return info
}

// fetchAppInfoSettings reads App Info's categories and age rating declaration.
func fetchAppInfoSettings(c *Client, r *Remote) error {
	if r.AppInfo == nil {
		return nil
	}
	rels := make([]string, len(categoryKeys))
	for i, k := range categoryKeys {
		rels[i] = k.Rel
	}
	doc, err := c.do("GET", "/v1/appInfos/"+r.AppInfo.ID, url.Values{"include": {strings.Join(rels, ",")}}, nil)
	if err != nil && err != errNotFound {
		return err
	}
	if err == nil && len(doc.Data) > 0 {
		info := &Resource{}
		if err := json.Unmarshal(doc.Data, info); err != nil {
			return err
		}
		for _, k := range categoryKeys {
			_, id := info.Related(k.Rel)
			r.Categories[k.File] = id
		}
	}
	r.AgeRating, err = c.GetOne("/v1/appInfos/" + r.AppInfo.ID + "/ageRatingDeclaration")
	return err
}

func jsonText(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func planAppInfo(p *Plan, l *localAppInfo, r *Remote, infoLock string) {
	if l == nil {
		return
	}
	if r.AppInfo == nil {
		p.Notes = append(p.Notes, appInfoFile+": the app has no App Info; skipped")
		return
	}
	lock := func(changes []*Change) bool {
		if infoLock != "" {
			p.lock(infoLock, changes...)
			return true
		}
		return false
	}

	// Categories: one PATCH of App Info's relationships.
	var changes []*Change
	rels := map[string]any{}
	for _, k := range categoryKeys {
		want, ok := l.Categories[k.File]
		if !ok || want == r.Categories[k.File] {
			continue
		}
		changes = append(changes, &Change{Label: "app_info/" + k.File, Old: r.Categories[k.File], New: want})
		if want == "" {
			rels[k.Rel] = map[string]any{"data": nil}
		} else {
			rels[k.Rel] = rel("appCategories", want)
		}
	}
	if len(changes) > 0 && !lock(changes) {
		id := r.AppInfo.ID
		p.add("update categories", func(c *Client) error {
			return c.PatchAt("/v1/appInfos/"+id, "appInfos", id, nil, rels)
		}, changes)
	}

	// Age rating: only the attributes the file lists.
	if l.AgeRating != nil {
		if r.AgeRating == nil {
			p.Notes = append(p.Notes, appInfoFile+": App Store Connect has no age rating declaration for this App Info; skipped")
		} else {
			var changes []*Change
			attrs := map[string]any{}
			for _, k := range sortedKeys(l.AgeRating) {
				want, have := l.AgeRating[k], r.AgeRating.Attributes[k]
				if reflect.DeepEqual(want, have) {
					continue
				}
				attrs[k] = want
				changes = append(changes, &Change{Label: "app_info/age_rating/" + k, Old: jsonText(have), New: jsonText(want)})
			}
			if len(changes) > 0 && !lock(changes) {
				id := r.AgeRating.ID
				p.add("update age rating", func(c *Client) error { return c.Patch("ageRatingDeclarations", id, attrs) }, changes)
			}
		}
	}

	// Content rights is on the app itself and can change at any time.
	if l.ContentRights.Set && l.ContentRights.Value != "" {
		if old := r.App.Str("contentRightsDeclaration"); old != l.ContentRights.Value {
			id, val := r.App.ID, l.ContentRights.Value
			p.add("update content rights", func(c *Client) error {
				return c.Patch("apps", id, map[string]any{"contentRightsDeclaration": val})
			}, []*Change{{Label: "app_info/content_rights", Old: old, New: val}})
		}
	}
}

// appInfoJSON renders app_info.json from App Store Connect, keys in a fixed order.
func appInfoJSON(r *Remote) string {
	var buf bytes.Buffer
	buf.WriteString("{\n")
	var lines []string
	for _, k := range categoryKeys {
		v := r.Categories[k.File]
		if v == "" && k.File != "secondary_category" {
			continue // an unset primary category (a new app) or subcategory isn't written
		}
		if v == "" {
			lines = append(lines, fmt.Sprintf("  %q: null", k.File))
		} else {
			lines = append(lines, fmt.Sprintf("  %q: %q", k.File, v))
		}
	}
	if v := r.App.Str("contentRightsDeclaration"); v != "" {
		lines = append(lines, fmt.Sprintf("  %q: %q", "content_rights", v))
	}
	if r.AgeRating != nil && len(r.AgeRating.Attributes) > 0 {
		keys := make([]string, 0, len(r.AgeRating.Attributes))
		for k := range r.AgeRating.Attributes {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var attrs []string
		for _, k := range keys {
			attrs = append(attrs, fmt.Sprintf("    %q: %s", k, jsonText(r.AgeRating.Attributes[k])))
		}
		lines = append(lines, "  \"age_rating\": {\n"+strings.Join(attrs, ",\n")+"\n  }")
	}
	buf.WriteString(strings.Join(lines, ",\n"))
	buf.WriteString("\n}\n")
	return buf.String()
}
