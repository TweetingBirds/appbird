package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

type recorded struct {
	Method, Path string
	Body         map[string]any
}

// fakeASC is a tiny in-memory App Store Connect with one app.
type fakeASC struct {
	mu              sync.Mutex
	server          *httptest.Server
	requests        []recorded
	resources       map[[2]string]map[string]any
	rels            map[[2]string]map[string]any
	children        map[string][]string // list path -> ids
	types           map[string]string   // list or single path -> resource type
	single          map[string]string   // single path -> id
	uploads         map[string][]byte
	created         int
	sawAuthOnUpload bool
	// fail makes the next n requests to "METHOD /path" fail: status > 0 answers with that
	// status, status -1 drops the connection.
	fail map[string]*failure
}

type failure struct{ status, times int }

func (f *fakeASC) failNext(method, path string, status, times int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail == nil {
		f.fail = map[string]*failure{}
	}
	f.fail[method+" "+path] = &failure{status, times}
}

var segment = map[string]string{"appAssetLibraryImages": "images", "appAssetLibraryVideos": "videos",
	"appAssetLibraryPlacements": "placements"}
var uploadable = setOf("appScreenshots", "appPreviews", "appAssetLibraryImages", "appAssetLibraryVideos")

func newFake(t *testing.T, versionState, infoState string) *fakeASC {
	f := &fakeASC{
		resources: map[[2]string]map[string]any{
			{"apps", "app1"}: {"bundleId": "com.example.app", "name": "Example"},
			{"appStoreVersions", "v1"}: {"versionString": "1.0", "appVersionState": versionState, "platform": "IOS",
				"copyright": "2026 Example"},
			{"appStoreVersionLocalizations", "vl1"}: {"locale": "en-US", "description": "Old description.\nSecond line.",
				"keywords": "a,b", "promotionalText": "Old promo", "whatsNew": nil,
				"supportUrl": "https://example.com/support", "marketingUrl": nil},
			{"appInfos", "i1"}: {"state": infoState},
			{"appInfoLocalizations", "il1"}: {"locale": "en-US", "name": "Example", "subtitle": "Old subtitle",
				"privacyPolicyUrl": "https://example.com/privacy"},
			{"appStoreReviewDetails", "rd1"}:          {"notes": "Old notes", "contactEmail": "secret@example.com"},
			{"subscriptionGroups", "g1"}:              {"referenceName": "Premium"},
			{"subscriptionGroupLocalizations", "gl1"}: {"locale": "en-US", "name": "Example Premium", "customAppName": nil},
			{"subscriptions", "s1"}:                   {"productId": "com.example.annual", "name": "Annual", "reviewNote": "Old note"},
			{"subscriptionLocalizations", "sl1"}:      {"locale": "en-US", "name": "Annual", "description": "Old"},
			{"endUserLicenseAgreements", "eula1"}:     {"agreementText": "Old EULA"},
			{"appAssetLibraries", "lib1"}:             {},
			{"inAppPurchases", "iap1"}: {"productId": "com.example.coins", "name": "Coins",
				"inAppPurchaseType": "CONSUMABLE", "reviewNote": "Old coin note"},
			{"inAppPurchases", "s1"}:                {"productId": "com.example.annual", "inAppPurchaseType": "AUTOMATICALLY_RENEWABLE_SUBSCRIPTION"},
			{"inAppPurchaseLocalizations", "iapl1"}: {"locale": "en-US", "name": "100 Coins", "description": "A pile of coins"},
		},
		rels:    map[[2]string]map[string]any{},
		uploads: map[string][]byte{},
		children: map[string][]string{
			"/v1/apps":                       {"app1"},
			"/v1/apps/app1/appStoreVersions": {"v1"},
			"/v1/appStoreVersions/v1/appStoreVersionLocalizations":     {"vl1"},
			"/v1/apps/app1/appInfos":                                   {"i1"},
			"/v1/appInfos/i1/appInfoLocalizations":                     {"il1"},
			"/v1/apps/app1/subscriptionGroups":                         {"g1"},
			"/v1/subscriptionGroups/g1/subscriptionGroupLocalizations": {"gl1"},
			"/v1/subscriptionGroups/g1/subscriptions":                  {"s1"},
			"/v1/subscriptions/s1/subscriptionLocalizations":           {"sl1"},
			"/v1/apps/app1/inAppPurchasesV2":                           {"iap1", "s1"},
			"/v1/inAppPurchases/iap1/inAppPurchaseLocalizations":       {"iapl1"},
		},
		single: map[string]string{
			"/v1/appStoreVersions/v1/appStoreReviewDetail": "rd1",
			"/v1/apps/app1/endUserLicenseAgreement":        "eula1",
			"/v1/apps/app1/assetLibrary":                   "lib1",
		},
		types: map[string]string{
			"/v1/apps": "apps", "/v1/apps/app1/appStoreVersions": "appStoreVersions",
			"/v1/appStoreVersions/v1/appStoreVersionLocalizations":     "appStoreVersionLocalizations",
			"/v1/apps/app1/appInfos":                                   "appInfos",
			"/v1/appInfos/i1/appInfoLocalizations":                     "appInfoLocalizations",
			"/v1/apps/app1/subscriptionGroups":                         "subscriptionGroups",
			"/v1/subscriptionGroups/g1/subscriptionGroupLocalizations": "subscriptionGroupLocalizations",
			"/v1/subscriptionGroups/g1/subscriptions":                  "subscriptions",
			"/v1/subscriptions/s1/subscriptionLocalizations":           "subscriptionLocalizations",
			"/v1/apps/app1/inAppPurchasesV2":                           "inAppPurchases",
			"/v1/inAppPurchases/iap1/inAppPurchaseLocalizations":       "inAppPurchaseLocalizations",
			"/v1/appStoreVersions/v1/appStoreReviewDetail":             "appStoreReviewDetails",
			"/v1/apps/app1/endUserLicenseAgreement":                    "endUserLicenseAgreements",
			"/v1/apps/app1/assetLibrary":                               "appAssetLibraries",
		},
	}
	f.server = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeASC) resource(rtype, id string) map[string]any {
	attrs := map[string]any{}
	for k, v := range f.resources[[2]string{rtype, id}] {
		attrs[k] = v
	}
	res := map[string]any{"type": rtype, "id": id, "attributes": attrs}
	if r, ok := f.rels[[2]string{rtype, id}]; ok {
		res["relationships"] = r
	}
	return res
}

func (f *fakeASC) attr(rtype, id, key string) any { return f.resources[[2]string{rtype, id}][key] }

func (f *fakeASC) addChild(parent, rtype, id string, attrs, rels map[string]any) {
	f.resources[[2]string{rtype, id}] = attrs
	f.children[parent] = append(f.children[parent], id)
	f.types[parent] = rtype
	if rels != nil {
		f.rels[[2]string{rtype, id}] = rels
	}
}

func (f *fakeASC) writes() []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []recorded
	for _, r := range f.requests {
		if r.Method != "GET" && r.Method != "PUT" {
			out = append(out, r)
		}
	}
	return out
}

func (f *fakeASC) reset() {
	f.mu.Lock()
	f.requests = nil
	f.mu.Unlock()
}

func reply(w http.ResponseWriter, status int, doc any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if doc != nil {
		_ = json.NewEncoder(w).Encode(doc)
	}
}

func (f *fakeASC) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := r.URL.Path
	// Resources under /v2 are stored with the /v1 ones.
	store := strings.Replace(path, "/v2/", "/v1/", 1)
	parts := strings.Split(strings.Trim(store, "/"), "/")
	raw, _ := io.ReadAll(r.Body)
	if parts[0] == "upload" {
		if r.Header.Get("Authorization") != "" {
			f.sawAuthOnUpload = true
		}
		f.requests = append(f.requests, recorded{r.Method, path, nil})
		f.uploads[parts[1]] = raw
		reply(w, 200, nil)
		return
	}
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		reply(w, 401, map[string]any{"errors": []any{map[string]any{"title": "no token"}}})
		return
	}
	var body map[string]any
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &body)
	}
	f.requests = append(f.requests, recorded{r.Method, path, body})
	if fl := f.fail[r.Method+" "+path]; fl != nil && fl.times > 0 {
		fl.times--
		if fl.status < 0 {
			if hj, ok := w.(http.Hijacker); ok {
				conn, _, _ := hj.Hijack()
				conn.Close()
				return
			}
		}
		reply(w, fl.status, map[string]any{"errors": []any{map[string]any{"title": "Injected failure"}}})
		return
	}

	switch r.Method {
	case "GET":
		if id, ok := f.single[store]; ok {
			reply(w, 200, map[string]any{"data": f.resource(f.types[store], id)})
			return
		}
		if ids, ok := f.children[store]; ok {
			data := []any{}
			included := []any{}
			for _, id := range ids {
				res := f.resource(f.types[store], id)
				data = append(data, res)
				if r.URL.Query().Get("include") != "" {
					for _, rel := range f.rels[[2]string{f.types[store], id}] {
						d := rel.(map[string]any)["data"].(map[string]any)
						included = append(included, f.resource(d["type"].(string), d["id"].(string)))
					}
				}
			}
			reply(w, 200, map[string]any{"data": data, "included": included, "links": map[string]any{}})
			return
		}
		if len(parts) == 3 {
			if _, ok := f.resources[[2]string{parts[1], parts[2]}]; ok {
				reply(w, 200, map[string]any{"data": f.resource(parts[1], parts[2])})
				return
			}
		}
		if len(parts) == 4 {
			reply(w, 200, map[string]any{"data": []any{}, "links": map[string]any{}})
			return
		}
		reply(w, 404, map[string]any{"errors": []any{map[string]any{"title": "Not found", "detail": path}}})
	case "PATCH":
		if len(parts) == 5 && parts[3] == "relationships" {
			var ids []string
			for _, d := range body["data"].([]any) {
				ids = append(ids, d.(map[string]any)["id"].(string))
			}
			f.children["/v1/"+parts[1]+"/"+parts[2]+"/"+parts[4]] = ids
			reply(w, 204, nil)
			return
		}
		key := [2]string{parts[1], parts[2]}
		attrs := body["data"].(map[string]any)["attributes"].(map[string]any)
		for k, v := range attrs {
			f.resources[key][k] = v
		}
		if attrs["uploaded"] == true {
			switch key[0] {
			case "appScreenshots":
				f.resources[key]["assetDeliveryState"] = map[string]any{"state": "COMPLETE"}
			case "appPreviews":
				f.resources[key]["videoDeliveryState"] = map[string]any{"state": "COMPLETE"}
			default:
				f.resources[key]["state"] = "PREPARE_FOR_SUBMISSION"
			}
		}
		reply(w, 200, map[string]any{"data": f.resource(key[0], key[1])})
	case "DELETE":
		key := [2]string{parts[1], parts[2]}
		delete(f.resources, key)
		for p, ids := range f.children {
			var kept []string
			for _, id := range ids {
				if id != key[1] {
					kept = append(kept, id)
				}
			}
			f.children[p] = kept
		}
		reply(w, 204, nil)
	case "POST":
		rtype := parts[1]
		f.created++
		id := "new" + strconv.Itoa(f.created)
		data := body["data"].(map[string]any)
		attrs := data["attributes"].(map[string]any)
		rels := data["relationships"].(map[string]any)
		var parentType, parentID string
		extra := map[string]any{}
		for name, rel := range rels {
			d := rel.(map[string]any)["data"].(map[string]any)
			if name == "image" || name == "video" {
				extra[name] = rel
				continue
			}
			parentType, parentID = d["type"].(string), d["id"].(string)
		}
		if uploadable[rtype] {
			size := int(attrs["fileSize"].(float64))
			attrs["uploadOperations"] = []any{map[string]any{"method": "PUT", "url": f.server.URL + "/upload/" + id,
				"offset": 0, "length": size, "requestHeaders": []any{map[string]any{"name": "Content-Type", "value": "application/octet-stream"}}}}
			attrs["state"] = "AWAITING_UPLOAD"
		}
		if rtype == "appStoreReviewDetails" {
			f.resources[[2]string{rtype, id}] = attrs
			f.single["/v1/"+parentType+"/"+parentID+"/appStoreReviewDetail"] = id
			f.types["/v1/"+parentType+"/"+parentID+"/appStoreReviewDetail"] = rtype
		} else {
			seg := segment[rtype]
			if seg == "" {
				seg = rtype
			}
			if len(extra) == 0 {
				extra = nil
			}
			f.addChild("/v1/"+parentType+"/"+parentID+"/"+seg, rtype, id, attrs, extra)
		}
		reply(w, 201, map[string]any{"data": f.resource(rtype, id)})
	default:
		reply(w, 405, nil)
	}
}
