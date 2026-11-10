package main

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"
)

// Change is one field that differs.
type Change struct {
	Label  string
	Old    string
	New    string
	Locked string // why it can't be pushed now, if it can't
}

// Operation is one write, covering one or more changes.
type Operation struct {
	Describe string
	Run      func(*Client) error
	Changes  []*Change
}

// Plan is everything that differs, and how to write it.
type Plan struct {
	Operations []*Operation
	Locked     []*Change
	Notes      []string
}

func (p *Plan) add(describe string, run func(*Client) error, changes []*Change) {
	if len(changes) > 0 {
		p.Operations = append(p.Operations, &Operation{describe, run, changes})
	}
}

func (p *Plan) lock(reason string, changes ...*Change) {
	for _, c := range changes {
		c.Locked = reason
		p.Locked = append(p.Locked, c)
	}
}

func (p *Plan) ChangeCount() int {
	n := 0
	for _, op := range p.Operations {
		n += len(op.Changes)
	}
	return n
}

// diffFields compares local values with a resource's attributes. Fields without a file are skipped.
func diffFields(prefix string, local map[string]string, remote *Resource, fields []field) (map[string]any, []*Change) {
	attrs := map[string]any{}
	var changes []*Change
	for _, f := range fields {
		value, ok := local[f.File]
		if !ok {
			continue
		}
		old := normalize(remote.Str(f.API))
		if old != value {
			if value == "" {
				attrs[f.API] = nil
			} else {
				attrs[f.API] = value
			}
			changes = append(changes, &Change{Label: prefix + f.File, Old: old, New: value})
		}
	}
	return attrs, changes
}

func createAttrs(locale string, attrs map[string]any) map[string]any {
	out := map[string]any{"locale": locale}
	for k, v := range attrs {
		if v != nil {
			out[k] = v
		}
	}
	return out
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func buildPlan(m *LocalMetadata, r *Remote) *Plan {
	p := &Plan{}
	vlabel := r.VersionString()
	versionLock := ""
	switch {
	case r.Version == nil:
		versionLock = "the app has no App Store version yet"
	case !r.VersionEditable:
		versionLock = fmt.Sprintf("version %s is %s; create the next version in App Store Connect to edit this", vlabel, versionState(r.Version))
	}
	infoLock := ""
	if !r.AppInfoEditable {
		infoLock = "App Info can only be edited while a new version is being prepared"
	}

	// App Info localizations: name, subtitle, privacy URLs.
	for _, loc := range sortedKeys(m.AppInfo) {
		values := m.AppInfo[loc]
		if len(values) == 0 {
			continue
		}
		existing := r.AppInfoLocs[loc]
		attrs, changes := diffFields(loc+"/", values, existing, appInfoFields)
		if len(changes) == 0 {
			continue
		}
		if infoLock != "" || r.AppInfo == nil {
			p.lock(firstNonEmpty(infoLock, "no App Info found"), changes...)
			continue
		}
		if existing != nil {
			id := existing.ID
			p.add("update App Info "+loc, func(c *Client) error { return c.Patch("appInfoLocalizations", id, attrs) }, changes)
		} else if values["name"] == "" {
			p.Notes = append(p.Notes, loc+": new App Store locales need name.txt; skipped")
		} else {
			infoID, a := r.AppInfo.ID, createAttrs(loc, attrs)
			p.add("add App Info locale "+loc, func(c *Client) error {
				_, err := c.Create("appInfoLocalizations", a, map[string]any{"appInfo": rel("appInfos", infoID)})
				return err
			}, changes)
		}
	}

	// Version localizations.
	for _, loc := range sortedKeys(m.Version) {
		values := m.Version[loc]
		if len(values) == 0 {
			continue
		}
		existing := r.VersionLocs[loc]
		attrs, changes := diffFields(loc+"/", values, existing, versionFields)
		if len(changes) == 0 {
			continue
		}
		if versionLock != "" {
			var pushable []*Change
			for _, ch := range changes {
				if liveEditableVersionFields[ch.Label[strings.LastIndex(ch.Label, "/")+1:]] {
					pushable = append(pushable, ch)
				} else {
					p.lock(versionLock, ch)
				}
			}
			if len(pushable) > 0 && existing != nil {
				id, a := existing.ID, map[string]any{"promotionalText": attrs["promotionalText"]}
				p.add(fmt.Sprintf("update %s %s (live version)", vlabel, loc),
					func(c *Client) error { return c.Patch("appStoreVersionLocalizations", id, a) }, pushable)
			} else if len(pushable) > 0 {
				p.lock(versionLock, pushable...)
			}
			continue
		}
		if existing != nil {
			id := existing.ID
			p.add(fmt.Sprintf("update %s %s", vlabel, loc),
				func(c *Client) error { return c.Patch("appStoreVersionLocalizations", id, attrs) }, changes)
		} else {
			vid, a := r.Version.ID, createAttrs(loc, attrs)
			p.add(fmt.Sprintf("add %s locale %s", vlabel, loc), func(c *Client) error {
				_, err := c.Create("appStoreVersionLocalizations", a, map[string]any{"appStoreVersion": rel("appStoreVersions", vid)})
				return err
			}, changes)
		}
	}

	// Copyright, a version attribute.
	if m.Copyright.Set && r.Version != nil {
		if old := normalize(r.Version.Str("copyright")); old != m.Copyright.Value {
			ch := &Change{Label: "copyright", Old: old, New: m.Copyright.Value}
			if versionLock != "" {
				p.lock(versionLock, ch)
			} else {
				vid, val := r.Version.ID, m.Copyright.Value
				p.add(fmt.Sprintf("update %s copyright", vlabel),
					func(c *Client) error {
						return c.Patch("appStoreVersions", vid, map[string]any{"copyright": nullable(val)})
					},
					[]*Change{ch})
			}
		}
	}

	// App Review notes. Only the notes: contact details stay in App Store Connect.
	if m.ReviewNotes.Set && r.Version != nil {
		old := normalize(r.ReviewDetail.Str("notes"))
		if old != m.ReviewNotes.Value {
			ch := &Change{Label: "review_information/notes", Old: old, New: m.ReviewNotes.Value}
			val := m.ReviewNotes.Value
			switch {
			case versionLock != "":
				p.lock(versionLock, ch)
			case r.ReviewDetail != nil:
				id := r.ReviewDetail.ID
				p.add(fmt.Sprintf("update %s App Review notes", vlabel),
					func(c *Client) error {
						return c.Patch("appStoreReviewDetails", id, map[string]any{"notes": nullable(val)})
					},
					[]*Change{ch})
			default:
				vid := r.Version.ID
				p.add(fmt.Sprintf("add %s App Review notes", vlabel), func(c *Client) error {
					_, err := c.Create("appStoreReviewDetails", map[string]any{"notes": val},
						map[string]any{"appStoreVersion": rel("appStoreVersions", vid)})
					return err
				}, []*Change{ch})
			}
		}
	}

	// Custom EULA: only an existing one is updated, since creating one means choosing territories.
	if m.EULA.Set {
		switch {
		case r.EULA == nil:
			if m.EULA.Value != "" {
				p.Notes = append(p.Notes, "eula.txt: the app uses Apple's standard EULA; add a custom one in App Store Connect first, then appbird can keep its text in sync")
			}
		case normalize(r.EULA.Str("agreementText")) != m.EULA.Value:
			if m.EULA.Value == "" {
				p.Notes = append(p.Notes, "eula.txt is empty; appbird won't blank the custom EULA")
				break
			}
			id, val := r.EULA.ID, m.EULA.Value
			p.add("update the custom EULA",
				func(c *Client) error {
					return c.Patch("endUserLicenseAgreements", id, map[string]any{"agreementText": val})
				},
				[]*Change{{Label: "eula", Old: normalize(r.EULA.Str("agreementText")), New: val}})
		}
	}

	// Subscriptions.
	for _, pid := range sortedKeys(m.Subscriptions) {
		local := m.Subscriptions[pid]
		remote := r.Subscriptions[pid]
		if remote == nil {
			p.Notes = append(p.Notes, "subscriptions/"+pid+": no such subscription in App Store Connect; skipped (found: "+
				firstNonEmpty(strings.Join(sortedKeys(r.Subscriptions), ", "), "none")+")")
			continue
		}
		if local.ReviewNote.Set {
			if old := normalize(remote.Sub.Str("reviewNote")); old != local.ReviewNote.Value {
				id, val := remote.Sub.ID, local.ReviewNote.Value
				p.add("update "+pid+" review note",
					func(c *Client) error {
						return c.Patch("subscriptions", id, map[string]any{"reviewNote": nullable(val)})
					},
					[]*Change{{Label: "subscriptions/" + pid + "/review_note", Old: old, New: val}})
			}
		}
		for _, loc := range sortedKeys(local.Locales) {
			values := local.Locales[loc]
			existing := remote.Locs[loc]
			attrs, changes := diffFields("subscriptions/"+pid+"/"+loc+"/", values, existing, subscriptionLocFields)
			switch {
			case len(changes) == 0:
			case existing != nil:
				id := existing.ID
				p.add("update "+pid+" "+loc, func(c *Client) error { return c.Patch("subscriptionLocalizations", id, attrs) }, changes)
			case values["name"] != "":
				sid, a := remote.Sub.ID, createAttrs(loc, attrs)
				p.add("add "+pid+" locale "+loc, func(c *Client) error {
					_, err := c.Create("subscriptionLocalizations", a, map[string]any{"subscription": rel("subscriptions", sid)})
					return err
				}, changes)
			default:
				p.Notes = append(p.Notes, "subscriptions/"+pid+"/"+loc+": a new locale needs name.txt; skipped")
			}
		}
	}
	for _, ref := range sortedKeys(m.SubscriptionGroups) {
		remote := r.Groups[ref]
		if remote == nil {
			p.Notes = append(p.Notes, "subscription_groups/"+ref+": no group with that reference name; skipped (found: "+
				firstNonEmpty(strings.Join(sortedKeys(r.Groups), ", "), "none")+")")
			continue
		}
		for _, loc := range sortedKeys(m.SubscriptionGroups[ref]) {
			values := m.SubscriptionGroups[ref][loc]
			existing := remote.Locs[loc]
			attrs, changes := diffFields("subscription_groups/"+ref+"/"+loc+"/", values, existing, subscriptionGroupLocFields)
			switch {
			case len(changes) == 0:
			case existing != nil:
				id := existing.ID
				p.add("update group "+ref+" "+loc, func(c *Client) error { return c.Patch("subscriptionGroupLocalizations", id, attrs) }, changes)
			case values["name"] != "":
				gid, a := remote.Group.ID, createAttrs(loc, attrs)
				p.add("add group "+ref+" locale "+loc, func(c *Client) error {
					_, err := c.Create("subscriptionGroupLocalizations", a, map[string]any{"subscriptionGroup": rel("subscriptionGroups", gid)})
					return err
				}, changes)
			default:
				p.Notes = append(p.Notes, "subscription_groups/"+ref+"/"+loc+": a new locale needs name.txt; skipped")
			}
		}
	}

	planTestFlight(p, m, r)

	var unmanaged []string
	for _, loc := range sortedKeys(r.VersionLocs) {
		if _, ok := m.Version[loc]; !ok {
			unmanaged = append(unmanaged, loc)
		}
	}
	if len(unmanaged) > 0 {
		p.Notes = append(p.Notes, "in App Store Connect but not in the repo (left alone): "+strings.Join(unmanaged, ", "))
	}
	return p
}

// planTestFlight writes testflight/: test information for the app, and What to Test for the
// newest build (or the one picked with --app-version and --build). TestFlight text can change
// at any time, so nothing here waits for an editable App Store version.
func planTestFlight(p *Plan, m *LocalMetadata, r *Remote) {
	blabel := r.buildLabel()
	for _, loc := range sortedKeys(m.BetaApp) {
		if values := m.BetaApp[loc]; len(values) > 0 {
			existing := r.BetaAppLocs[loc]
			attrs, changes := diffFields("testflight/"+loc+"/", values, existing, betaAppFields)
			switch {
			case len(changes) == 0:
			case existing != nil:
				id := existing.ID
				p.add("update TestFlight test information "+loc,
					func(c *Client) error { return c.Patch("betaAppLocalizations", id, attrs) }, changes)
			default:
				appID, a := r.App.ID, createAttrs(loc, attrs)
				p.add("add TestFlight test information locale "+loc, func(c *Client) error {
					_, err := c.Create("betaAppLocalizations", a, map[string]any{"app": rel("apps", appID)})
					return err
				}, changes)
			}
		}
		values := m.BetaBuild[loc]
		if len(values) == 0 {
			continue
		}
		existing := r.BuildLocs[loc]
		attrs, changes := diffFields("testflight/"+loc+"/", values, existing, betaBuildFields)
		switch {
		case len(changes) == 0:
		case r.Build == nil:
			p.lock("no TestFlight build found; upload one, or pick one with --build", changes...)
		case existing != nil:
			id := existing.ID
			p.add(fmt.Sprintf("update TestFlight build %s %s", blabel, loc),
				func(c *Client) error { return c.Patch("betaBuildLocalizations", id, attrs) }, changes)
		default:
			bid, a := r.Build.ID, createAttrs(loc, attrs)
			p.add(fmt.Sprintf("add TestFlight build %s locale %s", blabel, loc), func(c *Client) error {
				_, err := c.Create("betaBuildLocalizations", a, map[string]any{"build": rel("builds", bid)})
				return err
			}, changes)
		}
	}
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

// pull writes App Store Connect's text into metadata/ and returns the files it changed.
func pull(cfg Config, r *Remote, only map[string]bool) ([]string, error) {
	root := cfg.MetadataPath()
	var written []string
	put := func(value string, parts ...string) error {
		path := filepath.Join(append([]string{root}, parts...)...)
		changed, err := writeText(path, value)
		if changed {
			written = append(written, path)
		}
		return err
	}
	keep := func(loc string) bool { return len(only) == 0 || only[loc] }
	var err error
	try := func(e error) {
		if err == nil {
			err = e
		}
	}
	for _, loc := range sortedKeys(r.AppInfoLocs) {
		if !keep(loc) {
			continue
		}
		res := r.AppInfoLocs[loc]
		for _, f := range appInfoFields {
			if f.File == "privacy_choices_url" && res.Str(f.API) == "" {
				continue
			}
			try(put(res.Str(f.API), loc, f.File+".txt"))
		}
	}
	for _, loc := range sortedKeys(r.VersionLocs) {
		if !keep(loc) {
			continue
		}
		for _, f := range versionFields {
			try(put(r.VersionLocs[loc].Str(f.API), loc, f.File+".txt"))
		}
	}
	if r.Version != nil {
		try(put(r.Version.Str("copyright"), "copyright.txt"))
	}
	if r.EULA != nil {
		try(put(r.EULA.Str("agreementText"), "eula.txt"))
	}
	if r.ReviewDetail != nil {
		try(put(r.ReviewDetail.Str("notes"), "review_information", "notes.txt"))
	}
	for _, pid := range sortedKeys(r.Subscriptions) {
		s := r.Subscriptions[pid]
		try(put(s.Sub.Str("reviewNote"), "subscriptions", pid, "review_note.txt"))
		for _, loc := range sortedKeys(s.Locs) {
			if keep(loc) {
				for _, f := range subscriptionLocFields {
					try(put(s.Locs[loc].Str(f.API), "subscriptions", pid, loc, f.File+".txt"))
				}
			}
		}
	}
	for _, ref := range sortedKeys(r.Groups) {
		g := r.Groups[ref]
		for _, loc := range sortedKeys(g.Locs) {
			if keep(loc) {
				for _, f := range subscriptionGroupLocFields {
					try(put(g.Locs[loc].Str(f.API), "subscription_groups", ref, loc, f.File+".txt"))
				}
			}
		}
	}
	for _, loc := range sortedKeys(r.BetaAppLocs) {
		if keep(loc) {
			for _, f := range betaAppFields {
				if v := r.BetaAppLocs[loc].Str(f.API); v != "" || f.File == "description" {
					try(put(v, "testflight", loc, f.File+".txt"))
				}
			}
		}
	}
	for _, loc := range sortedKeys(r.BuildLocs) {
		if keep(loc) {
			for _, f := range betaBuildFields {
				try(put(r.BuildLocs[loc].Str(f.API), "testflight", loc, f.File+".txt"))
			}
		}
	}
	return written, err
}

// ---- output -------------------------------------------------------------------------------

func short(s string) string {
	return strings.ReplaceAll(s, "\n", " ⏎ ")
}

func printChange(w io.Writer, ch *Change) {
	if strings.Contains(ch.Old, "\n") || strings.Contains(ch.New, "\n") || len(ch.Old) > 70 || len(ch.New) > 70 {
		fmt.Fprintf(w, "  ~ %s\n", ch.Label)
		for _, line := range unifiedDiff(strings.Split(ch.Old, "\n"), strings.Split(ch.New, "\n")) {
			fmt.Fprintf(w, "      %s\n", line)
		}
		return
	}
	fmt.Fprintf(w, "  ~ %s: %q → %q\n", ch.Label, short(ch.Old), short(ch.New))
}

func printPlan(w io.Writer, p *Plan, r *Remote) {
	fmt.Fprintln(w, r.describe())
	if r.Build != nil {
		fmt.Fprintf(w, "TestFlight build %s\n", r.buildLabel())
	}
	if len(p.Operations) == 0 && len(p.Locked) == 0 {
		fmt.Fprintln(w, "No differences: App Store Connect matches the repo.")
	}
	for _, op := range p.Operations {
		fmt.Fprintf(w, "\n%s\n", op.Describe)
		for _, ch := range op.Changes {
			printChange(w, ch)
		}
	}
	var reasons []string
	byReason := map[string][]*Change{}
	for _, ch := range p.Locked {
		if _, ok := byReason[ch.Locked]; !ok {
			reasons = append(reasons, ch.Locked)
		}
		byReason[ch.Locked] = append(byReason[ch.Locked], ch)
	}
	for _, reason := range reasons {
		fmt.Fprintf(w, "\nDifferent, but can't be pushed now (%s):\n", reason)
		for _, ch := range byReason[reason] {
			printChange(w, ch)
		}
	}
	for _, n := range p.Notes {
		fmt.Fprintf(w, "\nnote: %s\n", n)
	}
}

// unifiedDiff is a small line diff (LCS) with one line of context, enough for listing copy.
func unifiedDiff(a, b []string) []string {
	n, m := len(a), len(b)
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else if lcs[i+1][j] >= lcs[i][j+1] {
				lcs[i][j] = lcs[i+1][j]
			} else {
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}
	type line struct {
		op   byte
		text string
	}
	var lines []line
	i, j := 0, 0
	for i < n || j < m {
		switch {
		case i < n && j < m && a[i] == b[j]:
			lines = append(lines, line{' ', a[i]})
			i, j = i+1, j+1
		case i < n && (j == m || lcs[i+1][j] >= lcs[i][j+1]):
			lines = append(lines, line{'-', a[i]})
			i++
		default:
			lines = append(lines, line{'+', b[j]})
			j++
		}
	}
	var out []string
	for k, l := range lines {
		if l.op != ' ' {
			out = append(out, string(l.op)+l.text)
			continue
		}
		near := (k > 0 && lines[k-1].op != ' ') || (k+1 < len(lines) && lines[k+1].op != ' ')
		if near {
			out = append(out, " "+l.text)
		} else if len(out) > 0 && out[len(out)-1] != "…" {
			out = append(out, "…")
		}
	}
	if len(out) > 0 && out[len(out)-1] == "…" {
		out = out[:len(out)-1]
	}
	return out
}
