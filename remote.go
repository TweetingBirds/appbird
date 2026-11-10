package main

import (
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
	Platform        string
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
	InAppPurchases  map[string]*remoteSubscription // product id; Sub is the in-app purchase
	Categories      map[string]string              // app_info.json key -> category ID
	AgeRating       *Resource
}

func (r *Remote) VersionString() string { return r.Version.Str("versionString") }

func byLocale(items []*Resource) map[string]*Resource {
	m := map[string]*Resource{}
	for _, i := range items {
		m[i.Str("locale")] = i
	}
	return m
}

func fetchRemote(c *Client, cfg Config, versionString string, withProducts bool) (*Remote, error) {
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
	r := &Remote{Platform: cfg.Platform, App: app, VersionLocs: map[string]*Resource{}, AppInfoLocs: map[string]*Resource{},
		Subscriptions: map[string]*remoteSubscription{}, Groups: map[string]*remoteGroup{},
		InAppPurchases: map[string]*remoteSubscription{}, Categories: map[string]string{}}

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
	if !withProducts {
		return r, nil
	}
	if err := fetchAppInfoSettings(c, r); err != nil {
		return nil, err
	}
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
	// Consumables, non-consumables and non-renewing subscriptions. Auto-renewable subscriptions
	// come back here too; they're handled above.
	iaps, err := c.List("/v1/apps/"+app.ID+"/inAppPurchasesV2", nil)
	if err != nil {
		return nil, err
	}
	for _, i := range iaps {
		if i.Str("inAppPurchaseType") == "AUTOMATICALLY_RENEWABLE_SUBSCRIPTION" {
			continue
		}
		locs, err := c.List("/v2/inAppPurchases/"+i.ID+"/inAppPurchaseLocalizations", nil)
		if err != nil {
			return nil, err
		}
		r.InAppPurchases[i.Str("productId")] = &remoteSubscription{Sub: i, Locs: byLocale(locs)}
	}
	return r, nil
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
