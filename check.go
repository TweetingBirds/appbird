package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

// `appbird check` answers one question: is this version ready to submit for review? It only
// reads. It checks what App Store Connect will have once the repo is pushed: a field the repo
// manages counts with the repo's value, and everything else with App Store Connect's.

// finding is one problem or warning. Field names the place (en-US/keywords, build, ...).
type finding struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

type readiness struct {
	First      bool // the platform has never had a version on the App Store
	Pending    int  // repo changes not yet in App Store Connect
	Problems   []finding
	Warnings   []finding
	NotChecked []string // what Apple's API doesn't show, so check can't either
}

func (rd *readiness) problem(field, format string, args ...any) {
	rd.Problems = append(rd.Problems, finding{field, fmt.Sprintf(format, args...)})
}

func (rd *readiness) warn(field, format string, args ...any) {
	rd.Warnings = append(rd.Warnings, finding{field, fmt.Sprintf(format, args...)})
}

func (rd *readiness) Ready() bool { return len(rd.Problems) == 0 }

// Versions that reached the App Store at some point. Any of these means this isn't the
// platform's first submission.
var releasedVersionStates = setOf("READY_FOR_DISTRIBUTION", "READY_FOR_SALE", "REMOVED_FROM_SALE",
	"DEVELOPER_REMOVED_FROM_SALE", "REPLACED_WITH_NEW_VERSION")

// Versions already handed to App Review, or approved and waiting to go out.
var submittedVersionStates = setOf("WAITING_FOR_REVIEW", "IN_REVIEW", "ACCEPTED", "PENDING_APPLE_RELEASE",
	"PENDING_DEVELOPER_RELEASE", "PROCESSING_FOR_DISTRIBUTION", "WAITING_FOR_EXPORT_COMPLIANCE")

// Age rating attributes that may be null on a finished questionnaire. gracRatingClassificationNumber
// is the classification number from Korea's Game Rating and Administration Committee, which only
// rated games have; App Store Connect shows Korea's rating without it.
var optionalAgeRatingAttrs = setOf("kidsAgeBand", "ageRatingOverride", "ageRatingOverrideV2",
	"koreaAgeRatingOverride", "gracRatingClassificationNumber", "developerAgeRatingInfoUrl",
	"seventeenPlus", "gamblingAndContests")

// checkExtras is what check reads beyond fetchRemote.
type checkExtras struct {
	Build         *Resource                        // the build selected for the version
	Screenshots   map[string]map[string]*remoteSet // locale -> display type
	PriceSchedule *Resource
	PriceErr      error
}

func fetchCheckExtras(c *Client, r *Remote) (*checkExtras, error) {
	x := &checkExtras{Screenshots: map[string]map[string]*remoteSet{}}
	if r.Version == nil {
		return x, nil
	}
	var err error
	if x.Build, err = c.GetOne("/v1/appStoreVersions/" + r.Version.ID + "/build"); err != nil {
		return nil, err
	}
	want := &LocalMedia{Screenshots: map[string]map[string][]string{}, Previews: map[string]map[string][]string{},
		Creative: map[string]map[string]string{}}
	for loc := range r.VersionLocs {
		want.Screenshots[loc] = map[string][]string{}
	}
	rm, err := fetchMedia(c, r, want)
	if err != nil {
		return nil, err
	}
	x.Screenshots = rm.ScreenshotSets
	// Read only. Appbird never writes prices.
	x.PriceSchedule, x.PriceErr = c.GetOne("/v1/apps/" + r.App.ID + "/appPriceSchedule")
	return x, nil
}

// localValue returns the repo's value when the repo manages the field, else App Store Connect's.
func localValue(local map[string]map[string]string, loc, file string, remote *Resource, api string) string {
	if v, ok := local[loc][file]; ok {
		return v
	}
	return normalize(remote.Str(api))
}

func checkReadiness(cfg Config, m *LocalMetadata, media *LocalMedia, r *Remote, x *checkExtras) *readiness {
	rd := &readiness{First: true}
	for _, v := range r.Versions {
		if v != r.Version && releasedVersionStates[versionState(v)] {
			rd.First = false
		}
	}

	// What's in the repo but wouldn't be accepted anyway.
	problems, warnings := validateLocal(cfg, m)
	for _, p := range problems {
		rd.problem("metadata", "%s", p)
	}
	for _, w := range warnings {
		rd.warn("metadata", "%s", w)
	}
	problems, warnings = validateMedia(cfg, media)
	for _, p := range problems {
		rd.problem("media", "%s", p)
	}
	for _, w := range warnings {
		rd.warn("media", "%s", w)
	}

	switch {
	case r.Version == nil:
		rd.problem("version", "%s has no App Store version yet. Create one with `appbird create-version 1.0`.", cfg.Platform)
		return rd
	case submittedVersionStates[versionState(r.Version)]:
		rd.problem("version", "%s is already submitted (%s).", r.VersionString(), versionState(r.Version))
		return rd
	case liveVersionStates[versionState(r.Version)]:
		rd.problem("version", "%s is already live (%s). Start the next one with `appbird create-version`.", r.VersionString(), versionState(r.Version))
		return rd
	case !r.VersionEditable:
		rd.problem("version", "%s is %s, which can't be submitted.", r.VersionString(), versionState(r.Version))
		return rd
	}

	if p := buildPlan(m, r); len(p.Operations) > 0 {
		rd.Pending = p.ChangeCount()
	}

	checkBuild(rd, x.Build)
	checkLocales(rd, m, r)
	checkScreenshots(rd, cfg, media, r, x)
	checkAppInfoSettings(rd, m, r)

	copyright := normalize(r.Version.Str("copyright"))
	if m.Copyright.Set {
		copyright = m.Copyright.Value
	}
	if copyright == "" {
		rd.problem("copyright", "missing. Add copyright.txt, such as \"2026 Example LLC\".")
	}

	checkReviewDetail(rd, r.ReviewDetail)
	checkProducts(rd, r)

	if rd.First {
		switch {
		case x.PriceErr != nil:
			rd.warn("price", "couldn't read the price schedule: %v", x.PriceErr)
		case x.PriceSchedule == nil:
			rd.problem("price", "no price set. Choose one (Free counts) in App Store Connect › Pricing and Availability. Appbird never sets prices.")
		}
		rd.NotChecked = append(rd.NotChecked,
			"App Privacy: the data collection answers and their publishing (Apple's API doesn't expose them)",
			"Availability: the countries and regions the app is sold in",
			"Trader status, if the app is distributed in the EU (Digital Services Act)")
	}
	return rd
}

func checkBuild(rd *readiness, b *Resource) {
	if b == nil {
		rd.problem("build", "no build selected for this version. Pick one in App Store Connect on the version's page.")
		return
	}
	label := "build " + b.Str("version")
	if state := b.Str("processingState"); state != "" && state != "VALID" {
		rd.problem("build", "%s is %s; App Review needs a VALID build.", label, state)
	}
	if expired, _ := b.Attributes["expired"].(bool); expired {
		rd.problem("build", "%s has expired. Upload a new one.", label)
	}
	if v, ok := b.Attributes["usesNonExemptEncryption"]; !ok || v == nil {
		rd.problem("build", "%s has no export compliance answer. Set ITSAppUsesNonExemptEncryption in Info.plist, or answer it in App Store Connect.", label)
	}
}

func checkLocales(rd *readiness, m *LocalMetadata, r *Remote) {
	versionLocs, infoLocs := map[string]bool{}, map[string]bool{}
	for l := range r.VersionLocs {
		versionLocs[l] = true
	}
	for l, v := range m.Version {
		if len(v) > 0 {
			versionLocs[l] = true
		}
	}
	for l := range r.AppInfoLocs {
		infoLocs[l] = true
	}
	for l, v := range m.AppInfo {
		if v["name"] != "" || r.AppInfoLocs[l] != nil {
			infoLocs[l] = true // push can only add an App Info locale with a name
		}
	}
	all := map[string]bool{}
	for l := range versionLocs {
		all[l] = true
	}
	for l := range infoLocs {
		all[l] = true
	}
	if primary := r.App.Str("primaryLocale"); primary != "" && !versionLocs[primary] {
		rd.problem(primary, "the app's primary language has no listing for this version.")
	}
	if len(all) == 0 {
		rd.problem("locales", "no listing in any language.")
	}

	for _, loc := range sortedKeys(all) {
		if !infoLocs[loc] {
			rd.problem(loc, "no App Info (name) in this language. Add %s/name.txt.", loc)
		} else {
			for _, f := range []struct{ file, api, what string }{
				{"name", "name", "the app name"},
				{"privacy_url", "privacyPolicyUrl", "a privacy policy URL"},
			} {
				if localValue(m.AppInfo, loc, f.file, r.AppInfoLocs[loc], f.api) == "" {
					rd.problem(loc+"/"+f.file, "missing: Apple requires %s.", f.what)
				}
			}
		}
		if !versionLocs[loc] {
			rd.problem(loc, "has App Info but no listing for version %s. Add %s/description.txt and the other version files.", r.VersionString(), loc)
			continue
		}
		vl := r.VersionLocs[loc]
		required := []struct{ file, api, what string }{
			{"description", "description", "a description"},
			{"keywords", "keywords", "keywords"},
			{"support_url", "supportUrl", "a support URL"},
		}
		if !rd.First {
			required = append(required, struct{ file, api, what string }{"release_notes", "whatsNew", "release notes (What's New) for an update"})
		}
		for _, f := range required {
			if localValue(m.Version, loc, f.file, vl, f.api) == "" {
				rd.problem(loc+"/"+f.file, "missing: Apple requires %s.", f.what)
			}
		}
		if rd.First && m.Version[loc]["release_notes"] != "" {
			rd.warn(loc+"/release_notes", "a first version has no What's New; App Store Connect won't take release_notes.txt until the next version.")
		}
	}
}

// Screenshot display types per platform: any one of `need` satisfies the requirement.
type shotRule struct {
	prefix string   // display types this rule covers
	need   []string // the sizes Apple requires
	device string
}

var shotRules = map[string][]shotRule{
	"IOS": {
		// App Store Connect now offers the 6.3-inch (Dynamic Island, medium display) set as the
		// iPhone base and scales it down, so it counts as well.
		{"APP_IPHONE_", []string{"APP_IPHONE_67", "APP_IPHONE_65", "APP_IPHONE_61"}, "iPhone"},
		{"APP_IPAD_", []string{"APP_IPAD_PRO_3GEN_129", "APP_IPAD_PRO_129"}, "iPad"},
	},
	"MAC_OS":    {{"APP_DESKTOP", []string{"APP_DESKTOP"}, "Mac"}},
	"TV_OS":     {{"APP_APPLE_TV", []string{"APP_APPLE_TV"}, "Apple TV"}},
	"VISION_OS": {{"APP_APPLE_VISION_PRO", []string{"APP_APPLE_VISION_PRO"}, "Apple Vision Pro"}},
}

var sizeNames = map[string]string{
	"APP_IPHONE_67": "6.9-inch", "APP_IPHONE_65": "6.5-inch", "APP_IPHONE_61": "6.3-inch",
	"APP_IPAD_PRO_3GEN_129": "13-inch", "APP_IPAD_PRO_129": "12.9-inch",
}

func checkScreenshots(rd *readiness, cfg Config, media *LocalMedia, r *Remote, x *checkExtras) {
	// Failed or unfinished uploads, in any language.
	for _, loc := range sortedKeys(x.Screenshots) {
		for _, t := range sortedKeys(x.Screenshots[loc]) {
			for i, item := range x.Screenshots[loc][t].Items {
				where := fmt.Sprintf("screenshots/%s/%s", loc, t)
				switch state := nestedState(item, "assetDeliveryState"); state {
				case "", "COMPLETE":
				case "FAILED":
					rd.problem(where, "screenshot %d failed to process. Replace it with `appbird media push`.", i+1)
				default:
					rd.warn(where, "screenshot %d is still processing (%s).", i+1, state)
				}
			}
		}
	}

	// Other languages fall back to the primary language's screenshots, so only it needs a full set.
	loc := r.App.Str("primaryLocale")
	if loc == "" {
		locs := sortedKeys(r.VersionLocs)
		if len(locs) == 0 {
			return
		}
		loc = locs[0]
	}
	counts := map[string]int{}
	for t, s := range x.Screenshots[loc] {
		counts[t] = len(s.Items)
	}
	unpushed := false
	for t, files := range media.Screenshots[loc] {
		if counts[t] != len(files) {
			unpushed = true
		}
		counts[t] = len(files) // the repo wins once pushed
	}
	if unpushed {
		rd.warn("screenshots/"+loc, "the repo's screenshots differ from App Store Connect's. Run `appbird media diff`, then `appbird media push`.")
	}

	rules := shotRules[cfg.Platform]
	found := 0
	for _, n := range counts {
		found += n
	}
	if found == 0 {
		rd.problem("screenshots/"+loc, "no screenshots in %s, the primary language. Add them under %s/screenshots/%s/ and run `appbird media push`.",
			loc, cfg.MetadataDir, loc)
		return
	}
	for _, rule := range rules {
		total, big := 0, 0
		for t, n := range counts {
			if strings.HasPrefix(t, rule.prefix) {
				total += n
			}
		}
		var sizes []string
		for _, t := range rule.need {
			big += counts[t]
			sizes = append(sizes, strings.TrimSpace(sizeNames[t]+" ("+t+")"))
		}
		need := strings.Join(sizes, " or ")
		switch {
		case total > 0 && big == 0:
			rd.warn("screenshots/"+loc, "no %s set for %s. App Store Connect prefers one and scales the smaller sizes from it.", need, rule.device)
		case total == 0 && len(rules) > 1:
			rd.warn("screenshots/"+loc, "no %s screenshots. Apple requires a %s set if the build runs on %s.", rule.device, need, rule.device)
		}
	}
}

func checkAppInfoSettings(rd *readiness, m *LocalMetadata, r *Remote) {
	if r.AppInfo == nil {
		rd.problem("app_info", "the app has no App Info in App Store Connect.")
		return
	}
	l := m.AppInfoSettings
	if l == nil {
		l = &localAppInfo{Categories: map[string]string{}}
	}
	primary, ok := l.Categories["primary_category"]
	if !ok {
		primary = r.Categories["primary_category"]
	}
	if primary == "" {
		rd.problem("app_info/primary_category", "missing. Set \"primary_category\" in %s, such as \"PRODUCTIVITY\".", appInfoFile)
	}
	rights := r.App.Str("contentRightsDeclaration")
	if l.ContentRights.Set && l.ContentRights.Value != "" {
		rights = l.ContentRights.Value
	}
	if rights == "" {
		rd.problem("app_info/content_rights", "missing. Set \"content_rights\" in %s to DOES_NOT_USE_THIRD_PARTY_CONTENT or USES_THIRD_PARTY_CONTENT.", appInfoFile)
	}

	attrs := map[string]any{}
	if r.AgeRating != nil {
		for k, v := range r.AgeRating.Attributes {
			attrs[k] = v
		}
	}
	for k, v := range l.AgeRating {
		attrs[k] = v
	}
	var unanswered []string
	answered := 0
	for k, v := range attrs {
		if optionalAgeRatingAttrs[k] {
			continue
		}
		if v == nil {
			unanswered = append(unanswered, k)
		} else {
			answered++
		}
	}
	sort.Strings(unanswered)
	switch {
	case answered == 0:
		rd.problem("app_info/age_rating", "the age rating questionnaire isn't answered. Fill in \"age_rating\" in %s, or answer it in App Store Connect.", appInfoFile)
	case len(unanswered) > 0:
		rd.warn("app_info/age_rating", "no answer for %s. Apple may have added these questions; answer them before submitting.", strings.Join(unanswered, ", "))
	}
}

func checkReviewDetail(rd *readiness, d *Resource) {
	where := "review contact"
	if d == nil {
		rd.problem(where, "no App Review information. Add a contact name, phone and email on the version's page in App Store Connect.")
		return
	}
	// Only whether each is set: contact details never leave App Store Connect.
	var missing []string
	for _, f := range []struct{ api, what string }{
		{"contactFirstName", "first name"}, {"contactLastName", "last name"},
		{"contactPhone", "phone"}, {"contactEmail", "email"},
	} {
		if strings.TrimSpace(d.Str(f.api)) == "" {
			missing = append(missing, f.what)
		}
	}
	if len(missing) > 0 {
		rd.problem(where, "missing %s. Add it under App Review Information in App Store Connect.", strings.Join(missing, ", "))
	}
	if required, _ := d.Attributes["demoAccountRequired"].(bool); required &&
		(strings.TrimSpace(d.Str("demoAccountName")) == "" || strings.TrimSpace(d.Str("demoAccountPassword")) == "") {
		rd.problem("review sign-in", "a demo account is marked required but its user name or password is empty.")
	}
}

func checkProducts(rd *readiness, r *Remote) {
	for _, kind := range []struct {
		dir      string
		products map[string]*remoteSubscription
	}{{"subscriptions", r.Subscriptions}, {"in_app_purchases", r.InAppPurchases}} {
		for _, pid := range sortedKeys(kind.products) {
			where := kind.dir + "/" + pid
			switch state := kind.products[pid].Sub.Str("state"); state {
			case "MISSING_METADATA":
				rd.warn(where, "is missing metadata (often the review screenshot or a price), so it can't go to App Review with this version.")
			case "DEVELOPER_ACTION_NEEDED", "REJECTED":
				rd.warn(where, "needs changes before App Review will take it (%s).", state)
			case "READY_TO_SUBMIT":
				rd.warn(where, "is ready but not submitted. To review it with this version, add it on the version's page in App Store Connect.")
			}
		}
	}
}

func printReadiness(w io.Writer, rd *readiness, r *Remote, x *checkExtras) {
	kind := "update"
	if rd.First {
		kind = "first submission"
	}
	if r.Version != nil {
		fmt.Fprintf(w, "%s %s %s (%s), %s\n", r.App.Str("name"), r.Platform, r.VersionString(), versionState(r.Version), kind)
	} else {
		fmt.Fprintf(w, "%s %s, no version yet\n", r.App.Str("name"), r.Platform)
	}
	if x != nil && x.Build != nil {
		fmt.Fprintf(w, "Build %s\n", x.Build.Str("version"))
	}
	if len(rd.Problems) > 0 {
		fmt.Fprintf(w, "\nProblems (%d):\n", len(rd.Problems))
		for _, f := range rd.Problems {
			fmt.Fprintf(w, "  ✗ %s: %s\n", f.Field, f.Message)
		}
	}
	if len(rd.Warnings) > 0 {
		fmt.Fprintf(w, "\nWarnings (%d):\n", len(rd.Warnings))
		for _, f := range rd.Warnings {
			fmt.Fprintf(w, "  ! %s: %s\n", f.Field, f.Message)
		}
	}
	if len(rd.NotChecked) > 0 {
		fmt.Fprintln(w, "\nCheck these in App Store Connect yourself:")
		for _, n := range rd.NotChecked {
			fmt.Fprintln(w, "  -", n)
		}
	}
	fmt.Fprintln(w)
	switch {
	case !rd.Ready():
		fmt.Fprintf(w, "Not ready to submit: %d problem(s).\n", len(rd.Problems))
	case rd.Pending > 0:
		fmt.Fprintf(w, "Ready once the repo is pushed: run `appbird push` for %d change(s), then submit in App Store Connect.\n", rd.Pending)
	default:
		fmt.Fprintln(w, "Ready to submit in App Store Connect. Appbird never submits for you.")
	}
}

func (a *app) cmdCheck(o *options) (int, error) {
	cfg, err := findConfig(".", o.config)
	if err != nil {
		return 1, err
	}
	m, err := loadLocal(cfg)
	if err != nil {
		var ue userError
		if !errors.As(err, &ue) {
			return 1, err
		}
		m = &LocalMetadata{} // no metadata/ folder: check App Store Connect alone
	}
	media := loadMedia(cfg)
	c, err := a.client(o)
	if err != nil {
		return 1, err
	}
	r, err := fetchRemote(c, cfg, o.appVersion, "", true)
	if err != nil {
		return 1, err
	}
	x, err := fetchCheckExtras(c, r)
	if err != nil {
		return 1, err
	}
	rd := checkReadiness(cfg, m, media, r, x)

	var buf bytes.Buffer
	printReadiness(&buf, rd, r, x)
	if a.json {
		if err := a.printJSON(checkJSON(rd, r)); err != nil {
			return 1, err
		}
	} else {
		fmt.Fprint(a.out, buf.String())
	}
	if path := os.Getenv("GITHUB_STEP_SUMMARY"); path != "" {
		if f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			fmt.Fprintf(f, "```\n%s```\n", buf.String())
			f.Close()
		}
	}
	if !rd.Ready() {
		return 1, nil
	}
	return 0, nil
}
