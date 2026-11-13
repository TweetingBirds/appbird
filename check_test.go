package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// makeReady fills in everything the fake app is missing for a first submission.
func makeReady(e *env) {
	f := e.fake
	f.resources[[2]string{"apps", "app1"}]["primaryLocale"] = "en-US"
	f.resources[[2]string{"apps", "app1"}]["contentRightsDeclaration"] = "DOES_NOT_USE_THIRD_PARTY_CONTENT"
	f.resources[[2]string{"builds", "b1"}]["usesNonExemptEncryption"] = false
	f.single["/v1/appStoreVersions/v1/build"] = "b1"
	f.types["/v1/appStoreVersions/v1/build"] = "builds"
	f.resources[[2]string{"appPriceSchedules", "ps1"}] = map[string]any{}
	f.single["/v1/apps/app1/appPriceSchedule"] = "ps1"
	f.types["/v1/apps/app1/appPriceSchedule"] = "appPriceSchedules"
	f.rels[[2]string{"appInfos", "i1"}] = map[string]any{
		"primaryCategory": map[string]any{"data": map[string]any{"type": "appCategories", "id": "PRODUCTIVITY"}}}
	for k, v := range map[string]any{"contactFirstName": "Ada", "contactLastName": "Lovelace", "contactPhone": "+1 555 0100"} {
		f.resources[[2]string{"appStoreReviewDetails", "rd1"}][k] = v
	}
	addShots(e, "ss1", "APP_IPHONE_67", "COMPLETE")
}

func addShots(e *env, id, displayType string, states ...string) {
	f := e.fake
	f.addChild("/v1/appStoreVersionLocalizations/vl1/appScreenshotSets", "appScreenshotSets", id,
		map[string]any{"screenshotDisplayType": displayType}, nil)
	for i, s := range states {
		f.addChild("/v1/appScreenshotSets/"+id+"/appScreenshots", "appScreenshots", id+"-"+string(rune('a'+i)),
			map[string]any{"assetDeliveryState": map[string]any{"state": s}}, nil)
	}
}

func TestCheckFirstSubmissionListsWhatsMissing(t *testing.T) {
	e := setup(t, "PREPARE_FOR_SUBMISSION", "PREPARE_FOR_SUBMISSION")
	e.fake.resources[[2]string{"apps", "app1"}]["primaryLocale"] = "en-US"
	code, out, stderr := e.run("check")
	must(t, code == 1, "exit %d: %s%s", code, out, stderr)
	contains(t, out, "IOS 1.0 (PREPARE_FOR_SUBMISSION), first submission")
	for _, want := range []string{
		"✗ build: no build selected",
		"✗ screenshots/en-US: no screenshots",
		"✗ app_info/primary_category: missing",
		"✗ app_info/content_rights: missing",
		"✗ review contact: missing first name, last name, phone",
		"✗ price: no price set",
		"App Privacy",
		"Not ready to submit: 6 problem(s).",
	} {
		contains(t, out, want)
	}
	// A first version has no What's New to require.
	must(t, !strings.Contains(out, "release_notes"), "no release notes on a first version:\n%s", out)
	must(t, !strings.Contains(out, "secret@example.com"), "review contact details are never printed")
	must(t, len(e.fake.writes()) == 0, "check only reads: %v", paths(e.fake.writes()))
}

func TestCheckReadyThenPendingThenMissing(t *testing.T) {
	e := setup(t, "PREPARE_FOR_SUBMISSION", "PREPARE_FOR_SUBMISSION")
	makeReady(e)
	// Optional attributes left empty on a finished questionnaire aren't flagged.
	e.fake.resources[[2]string{"ageRatingDeclarations", "ar1"}]["gracRatingClassificationNumber"] = nil
	code, out, _ := e.run("check")
	must(t, code == 0, "exit %d:\n%s", code, out)
	must(t, !strings.Contains(out, "age_rating"), "no age rating warning:\n%s", out)
	contains(t, out, "Build 45")
	contains(t, out, "! screenshots/en-US: no iPad screenshots")
	contains(t, out, "Ready to submit in App Store Connect")

	// The repo's values count as pushed.
	e.write("en-US/keywords.txt", "c,d\n")
	code, out, _ = e.run("check")
	must(t, code == 0, "exit %d:\n%s", code, out)
	contains(t, out, "Ready once the repo is pushed: run `appbird push` for 1 change(s)")

	e.write("en-US/keywords.txt", "")
	e.write("copyright.txt", "")
	code, out, _ = e.run("check")
	must(t, code == 1, "exit %d:\n%s", code, out)
	contains(t, out, "✗ en-US/keywords: missing")
	contains(t, out, "✗ copyright: missing")
	must(t, len(e.fake.writes()) == 0, "check only reads")
}

func TestCheckScreenshotSizesAndFailedUploads(t *testing.T) {
	e := setup(t, "PREPARE_FOR_SUBMISSION", "PREPARE_FOR_SUBMISSION")
	makeReady(e)
	e.fake.children["/v1/appStoreVersionLocalizations/vl1/appScreenshotSets"] = nil
	addShots(e, "ss2", "APP_IPHONE_55", "COMPLETE", "FAILED")
	addShots(e, "ss3", "APP_IPAD_PRO_3GEN_129", "COMPLETE")
	code, out, _ := e.run("check")
	must(t, code == 1, "exit %d:\n%s", code, out)
	contains(t, out, "✗ screenshots/en-US/APP_IPHONE_55: screenshot 2 failed to process")
	contains(t, out, "✗ screenshots/en-US: iPhone screenshots need a 6.9-inch (APP_IPHONE_67) or 6.5-inch (APP_IPHONE_65) or 6.3-inch (APP_IPHONE_61) set")
	must(t, !strings.Contains(out, "iPad screenshots need"), "13-inch iPad set is there:\n%s", out)

	// Screenshots in the repo count once pushed, with a reminder to push them.
	e.write("screenshots/en-US/APP_IPHONE_65/1.png", "png")
	_, out, _ = e.run("check")
	must(t, !strings.Contains(out, "iPhone screenshots need"), "6.5-inch set in the repo:\n%s", out)
	contains(t, out, "! screenshots/en-US: the repo's screenshots differ")

	// A 6.3-inch set alone is enough: App Store Connect scales it down.
	e2 := setup(t, "PREPARE_FOR_SUBMISSION", "PREPARE_FOR_SUBMISSION")
	makeReady(e2)
	e2.fake.children["/v1/appStoreVersionLocalizations/vl1/appScreenshotSets"] = nil
	addShots(e2, "ss4", "APP_IPHONE_61", "COMPLETE")
	addShots(e2, "ss5", "APP_IPAD_PRO_3GEN_129", "COMPLETE")
	code, out, _ = e2.run("check")
	must(t, code == 0 && !strings.Contains(out, "iPhone screenshots need"), "6.3-inch set: exit %d\n%s", code, out)
}

func TestCheckAnUpdateNeedsReleaseNotes(t *testing.T) {
	e := setup(t, "PREPARE_FOR_SUBMISSION", "PREPARE_FOR_SUBMISSION")
	makeReady(e)
	e.fake.addChild("/v1/apps/app1/appStoreVersions", "appStoreVersions", "v0",
		map[string]any{"versionString": "0.9", "appVersionState": "READY_FOR_SALE", "platform": "IOS"}, nil)
	e.fake.resources[[2]string{"inAppPurchases", "iap1"}]["state"] = "MISSING_METADATA"
	code, out, _ := e.run("check", "--json")
	must(t, code == 1, "exit %d:\n%s", code, out)
	var res jsonCheck
	must(t, json.Unmarshal([]byte(out), &res) == nil, "JSON:\n%s", out)
	must(t, !res.FirstSubmission && !res.Ready && res.Version.Version == "1.0", "%+v", res)
	must(t, len(res.Problems) == 1 && res.Problems[0].Field == "en-US/release_notes", "problems %+v", res.Problems)
	must(t, len(res.NotChecked) == 0, "first-submission reminders only on a first submission")
	found := false
	for _, w := range res.Warnings {
		found = found || (w.Field == "in_app_purchases/com.example.coins" && strings.Contains(w.Message, "missing metadata"))
	}
	must(t, found, "warnings %+v", res.Warnings)

	e.write("en-US/release_notes.txt", "Bug fixes.\n")
	code, out, _ = e.run("check")
	must(t, code == 0, "exit %d:\n%s", code, out)
}

func TestCheckALiveVersion(t *testing.T) {
	e := setup(t, "READY_FOR_DISTRIBUTION", "READY_FOR_DISTRIBUTION")
	code, out, _ := e.run("check")
	must(t, code == 1, "exit %d:\n%s", code, out)
	contains(t, out, "✗ version: 1.0 is already live (READY_FOR_DISTRIBUTION). Start the next one with `appbird create-version`.")
}
