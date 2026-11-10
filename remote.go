package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

func versionState(v *Resource) string {
	if s := v.Str("appVersionState"); s != "" {
		return s
	}
	if s := v.Str("appStoreState"); s != "" {
		return s
	}
	return "UNKNOWN"
}

func appInfoState(i *Resource) string {
	if s := i.Str("state"); s != "" {
		return s
	}
	if s := i.Str("appStoreState"); s != "" {
		return s
	}
	return "UNKNOWN"
}

type remoteSubscription struct {
	Sub  *Resource
	Locs map[string]*Resource
}

type remoteGroup struct {
	Group *Resource
	Locs  map[string]*Resource
}

// Remote is App Store Connect's current state for one app and version.
type Remote struct {
	App             *Resource
	Versions        []*Resource
	Version         *Resource
	VersionEditable bool
	AppInfo         *Resource
	AppInfoEditable bool
	VersionLocs     map[string]*Resource
	AppInfoLocs     map[string]*Resource
	ReviewDetail    *Resource
	EULA            *Resource
	Subscriptions   map[string]*remoteSubscription // product id
	Groups          map[string]*remoteGroup        // reference name
	Build           *Resource                      // the TestFlight build "What to Test" goes to
	BuildVersion    string                         // its version (CFBundleShortVersionString)
	BuildLocs       map[string]*Resource           // betaBuildLocalizations by locale
	BetaAppLocs     map[string]*Resource           // betaAppLocalizations by locale
}

func (r *Remote) VersionString() string { return r.Version.Str("versionString") }

func byLocale(items []*Resource) map[string]*Resource {
	m := map[string]*Resource{}
	for _, i := range items {
		m[i.Str("locale")] = i
	}
	return m
}

// fetchRemote reads the app's App Store state. full adds subscriptions and TestFlight, which
// the text commands need and status and media don't.
func fetchRemote(c *Client, cfg Config, versionString, buildNumber string, full bool) (*Remote, error) {
	apps, err := c.List("/v1/apps", url.Values{"filter[bundleId]": {cfg.BundleID}})
	if err != nil {
		return nil, err
	}
	var app *Resource
	for _, a := range apps {
		if a.Str("bundleId") == cfg.BundleID {
			app = a
			break
		}
	}
	if app == nil {
		return nil, userErr("No app with bundle ID %s is visible to this API key.", cfg.BundleID)
	}
	r := &Remote{App: app, VersionLocs: map[string]*Resource{}, AppInfoLocs: map[string]*Resource{},
		Subscriptions: map[string]*remoteSubscription{}, Groups: map[string]*remoteGroup{},
		BuildLocs: map[string]*Resource{}, BetaAppLocs: map[string]*Resource{}}

	if r.Versions, err = c.List("/v1/apps/"+app.ID+"/appStoreVersions", url.Values{"filter[platform]": {cfg.Platform}}); err != nil {
		return nil, err
	}
	if versionString != "" {
		var known []string
		for _, v := range r.Versions {
			known = append(known, v.Str("versionString"))
			if v.Str("versionString") == versionString && r.Version == nil {
				r.Version = v
			}
		}
		if r.Version == nil {
			return nil, userErr("No %s version %s in App Store Connect (have: %s).", cfg.Platform, versionString, strings.Join(known, ", "))
		}
	} else {
		for _, want := range []map[string]bool{editableVersionStates, liveVersionStates} {
			for _, v := range r.Versions {
				if want[versionState(v)] && r.Version == nil {
					r.Version = v
				}
			}
		}
		if r.Version == nil && len(r.Versions) > 0 {
			r.Version = r.Versions[0]
		}
	}
	r.VersionEditable = r.Version != nil && editableVersionStates[versionState(r.Version)]

	infos, err := c.List("/v1/apps/"+app.ID+"/appInfos", nil)
	if err != nil {
		return nil, err
	}
	for _, i := range infos {
		if editableAppInfoStates[appInfoState(i)] {
			r.AppInfo, r.AppInfoEditable = i, true
			break
		}
	}
	if r.AppInfo == nil && len(infos) > 0 {
		r.AppInfo = infos[0]
	}

	if r.Version != nil {
		locs, err := c.List("/v1/appStoreVersions/"+r.Version.ID+"/appStoreVersionLocalizations", nil)
		if err != nil {
			return nil, err
		}
		r.VersionLocs = byLocale(locs)
		if r.ReviewDetail, err = c.GetOne("/v1/appStoreVersions/" + r.Version.ID + "/appStoreReviewDetail"); err != nil {
			return nil, err
		}
	}
	if r.AppInfo != nil {
		locs, err := c.List("/v1/appInfos/"+r.AppInfo.ID+"/appInfoLocalizations", nil)
		if err != nil {
			return nil, err
		}
		r.AppInfoLocs = byLocale(locs)
	}
	if r.EULA, err = c.GetOne("/v1/apps/" + app.ID + "/endUserLicenseAgreement"); err != nil {
		return nil, err
	}
	if !full {
		return r, nil
	}
	if r.Build, r.BuildVersion, err = fetchBuild(c, cfg, app.ID, versionString, buildNumber); err != nil {
		return nil, err
	}
	if r.Build != nil {
		locs, err := c.List("/v1/builds/"+r.Build.ID+"/betaBuildLocalizations", nil)
		if err != nil {
			return nil, err
		}
		r.BuildLocs = byLocale(locs)
	}
	betaLocs, err := c.List("/v1/apps/"+app.ID+"/betaAppLocalizations", nil)
	if err != nil {
		return nil, err
	}
	r.BetaAppLocs = byLocale(betaLocs)
	groups, err := c.List("/v1/apps/"+app.ID+"/subscriptionGroups", nil)
	if err != nil {
		return nil, err
	}
	for _, g := range groups {
		ref := g.Str("referenceName")
		if ref == "" {
			ref = g.ID
		}
		glocs, err := c.List("/v1/subscriptionGroups/"+g.ID+"/subscriptionGroupLocalizations", nil)
		if err != nil {
			return nil, err
		}
		r.Groups[ref] = &remoteGroup{Group: g, Locs: byLocale(glocs)}
		subs, err := c.List("/v1/subscriptionGroups/"+g.ID+"/subscriptions", nil)
		if err != nil {
			return nil, err
		}
		for _, s := range subs {
			slocs, err := c.List("/v1/subscriptions/"+s.ID+"/subscriptionLocalizations", nil)
			if err != nil {
				return nil, err
			}
			r.Subscriptions[s.Str("productId")] = &remoteSubscription{Sub: s, Locs: byLocale(slocs)}
		}
	}
	return r, nil
}

// fetchBuild finds the newest unexpired TestFlight build on the config's platform: of one
// version if versionString is set, and with one build number if buildNumber is set.
// It returns nil when there isn't one.
func fetchBuild(c *Client, cfg Config, appID, versionString, buildNumber string) (*Resource, string, error) {
	params := url.Values{
		"filter[app]":                        {appID},
		"filter[preReleaseVersion.platform]": {cfg.Platform},
		"filter[expired]":                    {"false"},
		"sort":                               {"-uploadedDate"},
		"include":                            {"preReleaseVersion"},
		"limit":                              {"1"},
	}
	if versionString != "" {
		params.Set("filter[preReleaseVersion.version]", versionString)
	}
	if buildNumber != "" {
		params.Set("filter[version]", buildNumber)
	}
	// One page only: the first result is the newest.
	doc, err := c.do("GET", "/v1/builds", params, nil)
	if err == errNotFound {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	var builds []*Resource
	if len(doc.Data) > 0 && doc.Data[0] == '[' {
		if err := json.Unmarshal(doc.Data, &builds); err != nil {
			return nil, "", err
		}
	}
	if len(builds) == 0 {
		return nil, "", nil
	}
	b, v := builds[0], versionString
	if _, id := b.Related("preReleaseVersion"); id != "" {
		for _, inc := range doc.Included {
			if inc.Type == "preReleaseVersions" && inc.ID == id {
				v = inc.Str("version")
			}
		}
	}
	return b, v, nil
}

// buildLabel names the TestFlight build the way App Store Connect shows it: "1.2 (45)".
func (r *Remote) buildLabel() string {
	if r.Build == nil {
		return ""
	}
	if r.BuildVersion == "" {
		return "build " + r.Build.Str("version")
	}
	return fmt.Sprintf("%s (%s)", r.BuildVersion, r.Build.Str("version"))
}

func (r *Remote) describe() string {
	if r.Version == nil {
		return "No App Store version yet"
	}
	mode := "not editable"
	if r.VersionEditable {
		mode = "editable"
	}
	return fmt.Sprintf("Version %s (%s, %s)", r.VersionString(), versionState(r.Version), mode)
}
