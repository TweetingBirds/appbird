package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

type env struct {
	t    *testing.T
	fake *fakeASC
	repo string
	key  *ecdsa.PrivateKey
}

func setup(t *testing.T, versionState, infoState string) *env {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, _ := x509.MarshalPKCS8PrivateKey(key)
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "AuthKey_TEST.p8")
	os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600)
	fake := newFake(t, versionState, infoState)
	t.Setenv("ASC_KEY_ID", "TEST")
	t.Setenv("ASC_ISSUER_ID", "issuer")
	t.Setenv("ASC_PRIVATE_KEY_PATH", keyPath)
	t.Setenv("ASC_PRIVATE_KEY", "")
	t.Setenv("ASC_API_BASE", fake.server.URL)
	t.Setenv("GITHUB_STEP_SUMMARY", "")
	retryDelay = time.Millisecond
	repo := t.TempDir()
	os.WriteFile(filepath.Join(repo, "appbird.json"), []byte(`{"bundle_id": "com.example.app"}`), 0o644)
	old, _ := os.Getwd()
	os.Chdir(repo)
	t.Cleanup(func() { os.Chdir(old) })
	return &env{t, fake, repo, key}
}

func (e *env) run(args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := run(args, strings.NewReader(""), &out, &errb)
	return code, out.String(), errb.String()
}

func (e *env) write(rel, text string) string {
	path := filepath.Join(e.repo, "metadata", rel)
	os.MkdirAll(filepath.Dir(path), 0o755)
	os.WriteFile(path, []byte(text), 0o644)
	return path
}

func (e *env) read(rel string) string {
	b, _ := os.ReadFile(filepath.Join(e.repo, "metadata", rel))
	return string(b)
}

func must(t *testing.T, cond bool, msg string, args ...any) {
	t.Helper()
	if !cond {
		t.Fatalf(msg, args...)
	}
}

func contains(t *testing.T, s, sub string) {
	t.Helper()
	if !strings.Contains(s, sub) {
		t.Fatalf("expected %q in:\n%s", sub, s)
	}
}

func paths(rs []recorded) []string {
	var out []string
	for _, r := range rs {
		out = append(out, r.Method+" "+r.Path)
	}
	return out
}

func has(list []string, item string) bool {
	for _, i := range list {
		if i == item {
			return true
		}
	}
	return false
}

// ---- token ----------------------------------------------------------------------------------

func TestTokenIsAValidES256JWT(t *testing.T) {
	e := setup(t, "READY_FOR_DISTRIBUTION", "READY_FOR_DISTRIBUTION")
	creds, err := loadCredentials()
	must(t, err == nil, "credentials: %v", err)
	token, err := makeToken(creds, time.Unix(1000, 0))
	must(t, err == nil, "token: %v", err)
	parts := strings.Split(token, ".")
	must(t, len(parts) == 3, "three parts")
	header, _ := base64.RawURLEncoding.DecodeString(parts[0])
	payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
	must(t, string(header) == `{"alg":"ES256","kid":"TEST","typ":"JWT"}`, "header %s", header)
	var claims map[string]any
	json.Unmarshal(payload, &claims)
	must(t, claims["iss"] == "issuer" && claims["aud"] == "appstoreconnect-v1" && claims["iat"] == 1000.0 && claims["exp"] == 1900.0,
		"claims %v", claims)
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	must(t, len(sig) == 64, "raw r||s signature")
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	r, s := new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])
	must(t, ecdsa.Verify(&e.key.PublicKey, digest[:], r, s), "signature verifies")
}

func TestIndividualKeyAndKeyFromEnv(t *testing.T) {
	e := setup(t, "READY_FOR_DISTRIBUTION", "READY_FOR_DISTRIBUTION")
	pemBytes, _ := os.ReadFile(os.Getenv("ASC_PRIVATE_KEY_PATH"))
	t.Setenv("ASC_PRIVATE_KEY_PATH", "")
	t.Setenv("ASC_ISSUER_ID", "")
	t.Setenv("ASC_PRIVATE_KEY", strings.ReplaceAll(string(pemBytes), "\n", `\n`))
	creds, err := loadCredentials()
	must(t, err == nil, "credentials: %v", err)
	must(t, creds.Key.Equal(e.key), "same key")
	token, _ := makeToken(creds, time.Unix(1, 0))
	payload, _ := base64.RawURLEncoding.DecodeString(strings.Split(token, ".")[1])
	var claims map[string]any
	json.Unmarshal(payload, &claims)
	must(t, claims["sub"] == "user" && claims["iss"] == nil, "individual key claims %v", claims)
}

// ---- validate -------------------------------------------------------------------------------

func TestValidateLimitsURLsAndLocales(t *testing.T) {
	e := setup(t, "READY_FOR_DISTRIBUTION", "READY_FOR_DISTRIBUTION")
	e.write("en-US/name.txt", strings.Repeat("x", 31))
	e.write("en-US/support_url.txt", "not a url")
	e.write("es-419/description.txt", "Hola")
	e.write("en-US/stray.txt", "?")
	code, _, stderr := e.run("validate")
	must(t, code == 1, "exit %d", code)
	contains(t, stderr, "en-US/name: 31 characters, the limit is 30")
	contains(t, stderr, "not a URL")
	contains(t, stderr, "es-419: not a listing locale")
	contains(t, stderr, "ignored")
	must(t, len(e.fake.requests) == 0, "validate is offline")
}

func TestValidateCountsCharactersNotBytes(t *testing.T) {
	e := setup(t, "READY_FOR_DISTRIBUTION", "READY_FOR_DISTRIBUTION")
	e.write("ja/subtitle.txt", strings.Repeat("場", 30))
	code, out, stderr := e.run("validate")
	must(t, code == 0, "exit %d: %s", code, stderr)
	contains(t, out, "1 locale(s) (ja)")
}

// ---- live version ---------------------------------------------------------------------------

func TestDiffIsReadOnlyAndMarksLockedFields(t *testing.T) {
	e := setup(t, "READY_FOR_DISTRIBUTION", "READY_FOR_DISTRIBUTION")
	e.write("en-US/description.txt", "New description.\n")
	e.write("en-US/promotional_text.txt", "New promo\n")
	e.write("en-US/subtitle.txt", "New subtitle\n")
	code, out, _ := e.run("diff", "--exit-code")
	must(t, code == 2, "exit %d", code)
	must(t, len(e.fake.writes()) == 0, "diff wrote %v", paths(e.fake.writes()))
	contains(t, out, "READY_FOR_DISTRIBUTION, not editable")
	contains(t, out, `"Old promo" → "New promo"`)
	contains(t, out, "can't be pushed now (version 1.0 is READY_FOR_DISTRIBUTION")
	contains(t, out, "+New description.")
}

func TestPushOnALiveVersionSendsOnlyPromotionalText(t *testing.T) {
	e := setup(t, "READY_FOR_DISTRIBUTION", "READY_FOR_DISTRIBUTION")
	e.write("en-US/description.txt", "New description.\n")
	e.write("en-US/promotional_text.txt", "New promo\n")
	code, out, stderr := e.run("push", "--yes")
	must(t, code == 0, "exit %d: %s%s", code, out, stderr)
	w := e.fake.writes()
	must(t, len(w) == 1 && w[0].Path == "/v1/appStoreVersionLocalizations/vl1", "writes %v", paths(w))
	attrs := w[0].Body["data"].(map[string]any)["attributes"].(map[string]any)
	must(t, len(attrs) == 1 && attrs["promotionalText"] == "New promo", "attrs %v", attrs)
}

func TestPushNeedsYesWithoutATerminal(t *testing.T) {
	e := setup(t, "READY_FOR_DISTRIBUTION", "READY_FOR_DISTRIBUTION")
	e.write("en-US/promotional_text.txt", "New promo\n")
	code, _, stderr := e.run("push")
	must(t, code == 1, "exit %d", code)
	contains(t, stderr, "pass --yes")
	must(t, len(e.fake.writes()) == 0, "nothing written")
}

// ---- editable version -----------------------------------------------------------------------

func TestPushUpdatesCreatesAndNeverSubmits(t *testing.T) {
	e := setup(t, "PREPARE_FOR_SUBMISSION", "PREPARE_FOR_SUBMISSION")
	e.write("en-US/description.txt", "New description.\n")
	e.write("en-US/keywords.txt", "a,b\n")
	e.write("en-US/subtitle.txt", "New subtitle\n")
	e.write("en-US/marketing_url.txt", "")
	e.write("de-DE/name.txt", "Beispiel\n")
	e.write("de-DE/description.txt", "Neu.\n")
	e.write("copyright.txt", "2026 Example LLC\n")
	e.write("review_information/notes.txt", "New notes\n")
	e.write("subscriptions/com.example.annual/review_note.txt", "New note\n")
	e.write("subscriptions/com.example.annual/de-DE/name.txt", "Jährlich\n")
	e.write("subscription_groups/Premium/en-US/custom_app_name.txt", "Example\n")
	code, out, stderr := e.run("push", "--yes")
	must(t, code == 0, "exit %d: %s%s", code, out, stderr)
	got := paths(e.fake.writes())
	for _, want := range []string{
		"PATCH /v1/appStoreVersionLocalizations/vl1", "PATCH /v1/appInfoLocalizations/il1",
		"POST /v1/appInfoLocalizations", "POST /v1/appStoreVersionLocalizations", "PATCH /v1/appStoreVersions/v1",
		"PATCH /v1/appStoreReviewDetails/rd1", "PATCH /v1/subscriptions/s1", "POST /v1/subscriptionLocalizations",
		"PATCH /v1/subscriptionGroupLocalizations/gl1",
	} {
		must(t, has(got, want), "missing %s in %v", want, got)
	}
	for _, p := range got {
		must(t, !strings.Contains(p, "Submission") && !strings.HasPrefix(p, "DELETE"), "unexpected %s", p)
	}
	must(t, e.fake.attr("appStoreVersionLocalizations", "vl1", "description") == "New description.", "description written")
	must(t, e.fake.attr("appStoreReviewDetails", "rd1", "contactEmail") == "secret@example.com", "contact untouched")
	e.fake.reset()
	code, out, _ = e.run("diff", "--exit-code")
	must(t, code == 0, "second diff exit %d:\n%s", code, out)
	contains(t, out, "No differences")
}

func TestUnknownSubscriptionListsTheOnesThatExist(t *testing.T) {
	e := setup(t, "READY_FOR_DISTRIBUTION", "READY_FOR_DISTRIBUTION")
	e.write("subscriptions/com.example.monthly/review_note.txt", "Note\n")
	e.write("subscription_groups/Pro/en-US/name.txt", "Pro\n")
	code, out, _ := e.run("diff")
	must(t, code == 0, "exit %d", code)
	contains(t, out, "com.example.monthly: no such subscription in App Store Connect; skipped (found: com.example.annual)")
	contains(t, out, "Pro: no group with that reference name; skipped (found: Premium)")
}

func TestInAppPurchasesUpdateCreateAndPull(t *testing.T) {
	e := setup(t, "READY_FOR_DISTRIBUTION", "READY_FOR_DISTRIBUTION")
	code, _, stderr := e.run("pull")
	must(t, code == 0, "pull: %s", stderr)
	must(t, e.read("in_app_purchases/com.example.coins/en-US/name.txt") == "100 Coins\n", "IAP name pulled")
	must(t, e.read("in_app_purchases/com.example.coins/review_note.txt") == "Old coin note\n", "IAP review note pulled")
	_, err := os.Stat(filepath.Join(e.repo, "metadata/in_app_purchases/com.example.annual"))
	must(t, os.IsNotExist(err), "auto-renewable subscriptions stay under subscriptions/")

	e.write("in_app_purchases/com.example.coins/en-US/description.txt", "A big pile of coins\n")
	e.write("in_app_purchases/com.example.coins/de-DE/name.txt", "100 Münzen\n")
	e.write("in_app_purchases/com.example.coins/review_note.txt", "Buy, then check the balance\n")
	e.write("in_app_purchases/com.example.gems/en-US/name.txt", "Gems\n")
	e.fake.reset()
	code, out, stderr := e.run("push", "--yes")
	must(t, code == 0, "exit %d: %s%s", code, out, stderr)
	contains(t, out, "com.example.gems: no such in-app purchase in App Store Connect; skipped (found: com.example.coins)")
	got := paths(e.fake.writes())
	for _, want := range []string{"PATCH /v1/inAppPurchaseLocalizations/iapl1", "POST /v1/inAppPurchaseLocalizations", "PATCH /v2/inAppPurchases/iap1"} {
		must(t, has(got, want), "missing %s in %v", want, got)
	}
	for _, w := range e.fake.writes() {
		if w.Method == "POST" {
			rels := w.Body["data"].(map[string]any)["relationships"].(map[string]any)
			d := rels["inAppPurchaseV2"].(map[string]any)["data"].(map[string]any)
			must(t, d["type"] == "inAppPurchases" && d["id"] == "iap1", "related to the IAP: %v", rels)
		}
	}
	os.RemoveAll(filepath.Join(e.repo, "metadata/in_app_purchases/com.example.gems"))
	code, out, _ = e.run("diff", "--exit-code")
	must(t, code == 0, "clean after push:\n%s", out)

	e.write("in_app_purchases/com.example.coins/en-US/description.txt", strings.Repeat("x", 56))
	code, _, stderr = e.run("validate")
	must(t, code == 1, "exit %d", code)
	contains(t, stderr, "in_app_purchases/com.example.coins/en-US/description: 56 characters, the limit is 55")
}

func TestAppInfoCategoriesAgeRatingAndContentRights(t *testing.T) {
	e := setup(t, "PREPARE_FOR_SUBMISSION", "PREPARE_FOR_SUBMISSION")
	e.fake.rels[[2]string{"appInfos", "i1"}] = map[string]any{
		"primaryCategory":   map[string]any{"data": map[string]any{"type": "appCategories", "id": "PRODUCTIVITY"}},
		"secondaryCategory": map[string]any{"data": nil},
	}
	e.fake.resources[[2]string{"apps", "app1"}]["contentRightsDeclaration"] = "DOES_NOT_USE_THIRD_PARTY_CONTENT"
	code, _, stderr := e.run("pull")
	must(t, code == 0, "pull: %s", stderr)
	got := e.read("app_info.json")
	contains(t, got, `"primary_category": "PRODUCTIVITY"`)
	contains(t, got, `"secondary_category": null`)
	contains(t, got, `"content_rights": "DOES_NOT_USE_THIRD_PARTY_CONTENT"`)
	contains(t, got, `"violenceCartoonOrFantasy": "NONE"`)
	must(t, !strings.Contains(got, "subcategory"), "empty subcategories aren't written:\n%s", got)
	code, out, _ := e.run("diff", "--exit-code")
	must(t, code == 0, "clean after pull:\n%s", out)

	e.write("app_info.json", `{
  "primary_category": "GAMES",
  "primary_subcategory_one": "GAMES_PUZZLE",
  "secondary_category": "UTILITIES",
  "content_rights": "USES_THIRD_PARTY_CONTENT",
  "age_rating": {"violenceCartoonOrFantasy": "INFREQUENT_OR_MILD", "gambling": false, "lootBox": true}
}`)
	e.fake.reset()
	code, out, stderr = e.run("push", "--yes")
	must(t, code == 0, "exit %d: %s%s", code, out, stderr)
	contains(t, out, `app_info/primary_category: "PRODUCTIVITY" → "GAMES"`)
	contains(t, out, `app_info/age_rating/lootBox: "null" → "true"`)
	must(t, !strings.Contains(out, "age_rating/gambling"), "unchanged attributes aren't sent:\n%s", out)
	var patched map[string]any
	for _, w := range e.fake.writes() {
		if w.Path == "/v1/appInfos/i1" {
			patched = w.Body["data"].(map[string]any)["relationships"].(map[string]any)
		}
	}
	must(t, patched != nil && len(patched) == 3, "one PATCH with three relationships: %v", patched)
	ar := e.fake.resources[[2]string{"ageRatingDeclarations", "ar1"}]
	must(t, ar["violenceCartoonOrFantasy"] == "INFREQUENT_OR_MILD" && ar["lootBox"] == true, "age rating %v", ar)
	must(t, e.fake.attr("apps", "app1", "contentRightsDeclaration") == "USES_THIRD_PARTY_CONTENT", "content rights")
	code, out, _ = e.run("diff", "--exit-code")
	must(t, code == 0, "clean after push:\n%s", out)

	// On a live version, categories and the age rating wait; content rights doesn't.
	e.fake.resources[[2]string{"appInfos", "i1"}]["state"] = "READY_FOR_DISTRIBUTION"
	e.write("app_info.json", `{"primary_category": "BOOKS", "content_rights": "DOES_NOT_USE_THIRD_PARTY_CONTENT"}`)
	e.fake.reset()
	code, out, _ = e.run("push", "--yes")
	must(t, code == 0, "exit %d: %s", code, out)
	contains(t, out, "can't be pushed now")
	must(t, len(e.fake.writes()) == 1 && e.fake.writes()[0].Path == "/v1/apps/app1", "only content rights: %v", paths(e.fake.writes()))

	e.write("app_info.json", `{"primary_category": "books", "content_rights": "MAYBE", "age_rating": {"x": 3}, "price": 1}`)
	code, _, stderr = e.run("validate")
	must(t, code == 1, "exit %d", code)
	for _, want := range []string{"isn't an App Store category ID", "must be DOES_NOT_USE", "age_rating.x: must be a string", "price: not a key"} {
		contains(t, stderr, want)
	}
}

func TestNewAppStoreLocaleNeedsAName(t *testing.T) {
	e := setup(t, "PREPARE_FOR_SUBMISSION", "PREPARE_FOR_SUBMISSION")
	e.write("fr-FR/subtitle.txt", "Sous-titre\n")
	code, out, _ := e.run("push", "--yes")
	must(t, code == 0, "exit %d", code)
	contains(t, out, "fr-FR: new App Store locales need name.txt")
	must(t, len(e.fake.writes()) == 0, "nothing written")
}

func TestValidationFailureSendsNothing(t *testing.T) {
	e := setup(t, "PREPARE_FOR_SUBMISSION", "PREPARE_FOR_SUBMISSION")
	e.write("en-US/promotional_text.txt", strings.Repeat("x", 171))
	code, _, stderr := e.run("push", "--yes")
	must(t, code == 1, "exit %d", code)
	contains(t, stderr, "nothing was sent")
	must(t, len(e.fake.requests) == 0, "no requests at all")
}

func TestCreateVersion(t *testing.T) {
	e := setup(t, "READY_FOR_DISTRIBUTION", "READY_FOR_DISTRIBUTION")
	code, _, stderr := e.run("create-version", "1.0")
	must(t, code == 1, "exit %d", code)
	contains(t, stderr, "IOS version 1.0 already exists")
	code, _, stderr = e.run("create-version", "1.1")
	must(t, code == 1, "exit %d", code)
	contains(t, stderr, "pass --yes")
	code, _, _ = e.run("create-version", "v1.1")
	must(t, code == 2, "bad version number: exit %d", code)
	must(t, len(e.fake.writes()) == 0, "nothing written yet")

	code, out, stderr := e.run("create-version", "1.1", "--yes")
	must(t, code == 0, "exit %d: %s%s", code, out, stderr)
	contains(t, out, "Created version 1.1")
	w := e.fake.writes()
	must(t, len(w) == 1 && w[0].Method == "POST" && w[0].Path == "/v1/appStoreVersions", "one create: %v", paths(w))
	attrs := w[0].Body["data"].(map[string]any)["attributes"].(map[string]any)
	must(t, attrs["versionString"] == "1.1" && attrs["platform"] == "IOS", "attrs %v", attrs)

	_, out, _ = e.run("status")
	contains(t, out, "1.1  PREPARE_FOR_SUBMISSION ← editable")
	code, _, stderr = e.run("create-version", "1.2", "--yes")
	must(t, code == 1, "exit %d", code)
	contains(t, stderr, "Version 1.1 is already being prepared")
}

func TestJSONOutput(t *testing.T) {
	e := setup(t, "READY_FOR_DISTRIBUTION", "READY_FOR_DISTRIBUTION")
	code, out, _ := e.run("status", "--json")
	must(t, code == 0, "exit %d", code)
	var status jsonStatus
	must(t, json.Unmarshal([]byte(out), &status) == nil, "status is JSON:\n%s", out)
	must(t, status.App.BundleID == "com.example.app" && status.App.Platform == "IOS", "app %+v", status.App)
	must(t, len(status.Versions) == 1 && status.Versions[0].Live && !status.Versions[0].Editable, "versions %+v", status.Versions)
	must(t, status.Build != nil && status.Build.Version == "1.1" && status.Build.Build == "45", "build %+v", status.Build)

	e.write("en-US/promotional_text.txt", "New promo\n")
	e.write("en-US/subtitle.txt", "New subtitle\n")
	code, out, _ = e.run("diff", "--json", "--exit-code")
	must(t, code == 2, "exit %d", code)
	var plan jsonPlan
	must(t, json.Unmarshal([]byte(out), &plan) == nil, "diff is JSON:\n%s", out)
	must(t, plan.Differs && plan.ChangeCount == 1 && plan.Version.Version == "1.0", "plan %+v", plan)
	must(t, plan.Build != nil && plan.Build.Build == "45", "plan build %+v", plan.Build)
	must(t, plan.Steps[0].Changes[0].Field == "en-US/promotional_text" && plan.Steps[0].Changes[0].New == "New promo", "steps %+v", plan.Steps)
	must(t, len(plan.Waiting) == 1 && plan.Waiting[0].Field == "en-US/subtitle" && plan.Waiting[0].Reason != "", "waiting %+v", plan.Waiting)

	code, _, stderr := e.run("push", "--json")
	must(t, code == 2, "push --json: exit %d", code)
	contains(t, stderr, "--json works with status, diff, check and media diff")
}

// ---- failures -------------------------------------------------------------------------------

func TestPushRetriesAPatchAfterAServerError(t *testing.T) {
	e := setup(t, "PREPARE_FOR_SUBMISSION", "PREPARE_FOR_SUBMISSION")
	e.write("en-US/description.txt", "New description.\n")
	e.fake.failNext("PATCH", "/v1/appStoreVersionLocalizations/vl1", 500, 2)
	code, out, stderr := e.run("push", "--yes")
	must(t, code == 0, "exit %d: %s%s", code, out, stderr)
	must(t, e.fake.attr("appStoreVersionLocalizations", "vl1", "description") == "New description.", "written on the third try")
}

func TestPushNeverRetriesACreateAndReportsWhatWasWritten(t *testing.T) {
	for _, status := range []int{500, -1} {
		e := setup(t, "PREPARE_FOR_SUBMISSION", "PREPARE_FOR_SUBMISSION")
		e.write("en-US/description.txt", "New description.\n")
		e.write("de-DE/description.txt", "Neu.\n")
		e.write("fr-FR/name.txt", "Exemple\n")
		e.fake.failNext("POST", "/v1/appInfoLocalizations", status, 1)
		code, out, stderr := e.run("push", "--yes")
		must(t, code == 1, "status %d: exit %d", status, code)
		posts := 0
		for _, w := range e.fake.writes() {
			if w.Method == "POST" && w.Path == "/v1/appInfoLocalizations" {
				posts++
			}
		}
		must(t, posts == 1, "status %d: the create was sent %d times", status, posts)
		contains(t, out, "→ add App Info locale fr-FR")
		contains(t, stderr, "Stopped at step 1 of 3: add App Info locale fr-FR")
		contains(t, stderr, "✗ update 1.0 en-US")
		contains(t, stderr, "may or may not have reached App Store Connect")
		contains(t, stderr, "0 of 3 step(s) written")
	}
}

func TestPushReportsStepsWrittenBeforeAFailure(t *testing.T) {
	e := setup(t, "PREPARE_FOR_SUBMISSION", "PREPARE_FOR_SUBMISSION")
	e.write("en-US/subtitle.txt", "New subtitle\n")
	e.write("en-US/description.txt", "New description.\n")
	e.fake.failNext("PATCH", "/v1/appStoreVersionLocalizations/vl1", 409, 1)
	code, _, stderr := e.run("push", "--yes")
	must(t, code == 1, "exit %d", code)
	contains(t, stderr, "✓ update App Info en-US")
	contains(t, stderr, "Injected failure")
	contains(t, stderr, "1 of 2 step(s) written")
	must(t, !strings.Contains(stderr, "may or may not"), "a 409 is a clear refusal")
	// Pushing again only sends what's left.
	e.fake.reset()
	code, out, stderr := e.run("push", "--yes")
	must(t, code == 0, "exit %d: %s%s", code, out, stderr)
	must(t, len(e.fake.writes()) == 1, "only the failed step again: %v", paths(e.fake.writes()))
}

// ---- pull and EULA --------------------------------------------------------------------------

func TestPullThenDiffIsClean(t *testing.T) {
	e := setup(t, "READY_FOR_DISTRIBUTION", "READY_FOR_DISTRIBUTION")
	code, _, stderr := e.run("pull")
	must(t, code == 0, "pull: %s", stderr)
	must(t, e.read("en-US/description.txt") == "Old description.\nSecond line.\n", "description")
	must(t, e.read("en-US/marketing_url.txt") == "", "empty field becomes an empty file")
	must(t, e.read("en-US/name.txt") == "Example\n", "name")
	must(t, e.read("review_information/notes.txt") == "Old notes\n", "notes")
	must(t, e.read("eula.txt") == "Old EULA\n", "eula")
	must(t, e.read("subscriptions/com.example.annual/en-US/name.txt") == "Annual\n", "subscription")
	must(t, e.read("testflight/en-US/what_to_test.txt") == "Old test notes\n", "what to test")
	must(t, e.read("testflight/en-US/feedback_email.txt") == "beta@example.com\n", "feedback email")
	_, err := os.Stat(filepath.Join(e.repo, "metadata/testflight/en-US/marketing_url.txt"))
	must(t, os.IsNotExist(err), "empty beta URLs aren't pulled")
	_, err = os.Stat(filepath.Join(e.repo, "metadata/review_information/contact_email.txt"))
	must(t, os.IsNotExist(err), "contact details never pulled")
	must(t, len(e.fake.writes()) == 0, "pull doesn't write to App Store Connect")
	code, out, _ := e.run("diff", "--exit-code")
	must(t, code == 0, "diff after pull:\n%s", out)
	_, out, _ = e.run("pull")
	contains(t, out, "already matches")
}

func TestEULAUpdatesOnALiveVersionAndIsNeverBlanked(t *testing.T) {
	e := setup(t, "READY_FOR_DISTRIBUTION", "READY_FOR_DISTRIBUTION")
	e.write("eula.txt", "New EULA\n")
	code, out, _ := e.run("push", "--yes")
	must(t, code == 0, "exit %d: %s", code, out)
	must(t, e.fake.attr("endUserLicenseAgreements", "eula1", "agreementText") == "New EULA", "eula written")
	e.write("eula.txt", "")
	_, out, _ = e.run("push", "--yes")
	contains(t, out, "won't blank the custom EULA")
}

// ---- media ----------------------------------------------------------------------------------

func png(w, h int, alpha bool, salt string) string {
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:], uint32(w))
	binary.BigEndian.PutUint32(ihdr[4:], uint32(h))
	ihdr[8] = 8
	ihdr[9] = 2
	if alpha {
		ihdr[9] = 6
	}
	return "\x89PNG\r\n\x1a\n\x00\x00\x00\x0dIHDR" + string(ihdr) + "\x00\x00\x00\x00" + salt
}

func (e *env) shotsIn(setID string) []string {
	var names []string
	for _, id := range e.fake.children["/v1/appScreenshotSets/"+setID+"/appScreenshots"] {
		names = append(names, e.fake.attr("appScreenshots", id, "fileName").(string))
	}
	return names
}

func TestScreenshotsCreateNoopReplaceAndReorder(t *testing.T) {
	e := setup(t, "PREPARE_FOR_SUBMISSION", "PREPARE_FOR_SUBMISSION")
	a := e.write("screenshots/en-US/APP_IPHONE_67/01-journal.png", png(1320, 2868, false, "a"))
	e.write("screenshots/en-US/APP_IPHONE_67/02-map.png", png(1320, 2868, false, "b"))
	code, out, stderr := e.run("media", "push", "--yes")
	must(t, code == 0, "exit %d: %s%s", code, out, stderr)
	sets := e.fake.children["/v1/appStoreVersionLocalizations/vl1/appScreenshotSets"]
	must(t, len(sets) == 1, "one set: %v", sets)
	set := sets[0]
	must(t, e.fake.attr("appScreenshotSets", set, "screenshotDisplayType") == "APP_IPHONE_67", "display type")
	must(t, strings.Join(e.shotsIn(set), ",") == "01-journal.png,02-map.png", "order %v", e.shotsIn(set))
	must(t, len(e.fake.uploads) == 2 && !e.fake.sawAuthOnUpload, "two unauthenticated uploads")
	sum, _ := fileMD5(a)
	found := false
	for _, id := range e.fake.children["/v1/appScreenshotSets/"+set+"/appScreenshots"] {
		found = found || e.fake.attr("appScreenshots", id, "sourceFileChecksum") == sum
	}
	must(t, found, "checksum sent")

	e.fake.reset()
	code, out, _ = e.run("media", "diff", "--exit-code")
	must(t, code == 0, "noop diff:\n%s", out)
	contains(t, out, "No differences")

	// Rename 01 to 03 (same bytes: kept and moved) and change 02.
	os.Rename(a, filepath.Join(filepath.Dir(a), "03-journal.png"))
	e.write("screenshots/en-US/APP_IPHONE_67/02-map.png", png(1320, 2868, false, "c"))
	code, out, stderr = e.run("media", "push", "--yes")
	must(t, code == 0, "exit %d: %s%s", code, out, stderr)
	contains(t, out, "1 upload, 1 remove")
	must(t, len(e.fake.uploads) == 3, "one more upload, got %d", len(e.fake.uploads))
	must(t, strings.Join(e.shotsIn(set), ",") == "02-map.png,01-journal.png", "order %v", e.shotsIn(set))
}

func TestMediaPullMatchesWhatWasUploaded(t *testing.T) {
	e := setup(t, "PREPARE_FOR_SUBMISSION", "PREPARE_FOR_SUBMISSION")
	e.write("screenshots/en-US/APP_IPHONE_67/journal.png", png(1320, 2868, false, "a"))
	e.write("screenshots/en-US/APP_IPHONE_67/map.png", png(1320, 2868, false, "b"))
	e.write("previews/en-US/IPHONE_67/01-tour.mov", "movie")
	code, out, stderr := e.run("media", "push", "--yes")
	must(t, code == 0, "push: %d %s%s", code, out, stderr)
	os.RemoveAll(filepath.Join(e.repo, "metadata/screenshots"))
	os.RemoveAll(filepath.Join(e.repo, "metadata/previews"))

	e.fake.reset()
	code, out, stderr = e.run("media", "pull")
	must(t, code == 0, "pull: %d %s%s", code, out, stderr)
	must(t, len(e.fake.writes()) == 0, "pull doesn't write: %v", paths(e.fake.writes()))
	must(t, !e.fake.sawAuthOnUpload, "CDN downloads carry no token")
	contains(t, out, "+ metadata/screenshots/en-US/APP_IPHONE_67/01-journal.png")
	contains(t, out, "+ metadata/screenshots/en-US/APP_IPHONE_67/02-map.png")
	contains(t, out, "+ metadata/previews/en-US/IPHONE_67/01-tour.mov")
	contains(t, e.read("screenshots/en-US/APP_IPHONE_67/01-journal.png"), "re-encoded")

	// The downloads differ from the uploads, but count as them.
	code, out, _ = e.run("media", "diff", "--exit-code")
	must(t, code == 0, "diff after pull:\n%s", out)
	_, out, _ = e.run("media", "pull")
	contains(t, out, "already matches")

	// Changing a pulled file makes it a new upload.
	e.write("screenshots/en-US/APP_IPHONE_67/02-map.png", png(1320, 2868, false, "c"))
	code, out, _ = e.run("media", "diff", "--exit-code")
	must(t, code == 2, "diff after an edit: %d\n%s", code, out)
	contains(t, out, "1 upload, 1 remove")

	// Pulling again puts App Store Connect's set back.
	_, out, _ = e.run("media", "pull", "--only", "screenshots")
	contains(t, out, "Wrote 2 file(s)")
	code, out, _ = e.run("media", "diff", "--exit-code")
	must(t, code == 0, "diff after the second pull:\n%s", out)
}

func TestScreenshotsWaitForAnEditableVersion(t *testing.T) {
	e := setup(t, "READY_FOR_DISTRIBUTION", "READY_FOR_DISTRIBUTION")
	e.write("screenshots/en-US/APP_IPHONE_67/01.png", png(1320, 2868, false, ""))
	code, out, _ := e.run("media", "push", "--yes")
	must(t, code == 0, "exit %d", code)
	contains(t, out, "change only on a version being prepared")
	must(t, len(e.fake.writes()) == 0, "nothing written: %v", paths(e.fake.writes()))
}

func TestCreativeAssetsUploadOncePlaceEverywhereAndReplace(t *testing.T) {
	e := setup(t, "PREPARE_FOR_SUBMISSION", "PREPARE_FOR_SUBMISSION")
	e.fake.addChild("/v1/appStoreVersions/v1/appStoreVersionLocalizations", "appStoreVersionLocalizations", "vl2",
		map[string]any{"locale": "de-DE"}, nil)
	e.write("creative_assets/header.png", png(3840, 1646, false, "h"))
	e.write("creative_assets/de-DE/search_results.png", png(3840, 2560, false, "s"))
	code, out, stderr := e.run("media", "push", "--yes")
	must(t, code == 0, "exit %d: %s%s", code, out, stderr)
	contains(t, out, "Asset Library")
	must(t, len(e.fake.children["/v1/appAssetLibraries/lib1/images"]) == 2, "header uploaded once for both locales")
	must(t, len(e.fake.children["/v1/appStoreVersionLocalizations/vl1/placements"]) == 1, "en-US gets the header")
	var types []string
	for _, id := range e.fake.children["/v1/appStoreVersionLocalizations/vl2/placements"] {
		types = append(types, e.fake.attr("appAssetLibraryPlacements", id, "placementType").(string))
	}
	sort.Strings(types)
	must(t, strings.Join(types, ",") == "APP_STORE_SEARCH_RESULTS_ASSET,PRODUCT_PAGE_HEADER_ASSET", "de-DE placements %v", types)

	code, out, _ = e.run("media", "diff", "--exit-code")
	must(t, code == 0, "noop diff:\n%s", out)

	e.write("creative_assets/header.png", png(3840, 1646, false, "h2"))
	e.fake.reset()
	code, out, stderr = e.run("media", "push", "--yes")
	must(t, code == 0, "exit %d: %s%s", code, out, stderr)
	must(t, len(e.fake.children["/v1/appAssetLibraries/lib1/images"]) == 3, "one new image")
	must(t, len(e.fake.children["/v1/appStoreVersionLocalizations/vl1/placements"]) == 1, "still one placement on en-US")
	var deletes []string
	for _, w := range e.fake.writes() {
		if w.Method == "DELETE" {
			deletes = append(deletes, w.Path)
		}
	}
	must(t, len(deletes) == 2, "two old header placements removed: %v", deletes)
	for _, d := range deletes {
		must(t, strings.HasPrefix(d, "/v1/appAssetLibraryPlacements/"), "only placements deleted: %s", d)
	}
}

func TestMediaValidation(t *testing.T) {
	e := setup(t, "PREPARE_FOR_SUBMISSION", "PREPARE_FOR_SUBMISSION")
	e.write("creative_assets/header.png", png(3840, 1646, true, ""))
	e.write("creative_assets/search_results.png", png(1000, 1000, false, ""))
	for i := 0; i < 11; i++ {
		e.write("screenshots/en-US/APP_IPHONE_DUO/"+string(rune('a'+i))+".png", png(1398, 2034, false, string(rune('a'+i))))
	}
	code, _, stderr := e.run("media", "validate")
	must(t, code == 1, "exit %d", code)
	contains(t, stderr, "alpha channel")
	contains(t, stderr, "1000x1000 isn't one of Apple's sizes")
	contains(t, stderr, "APP_IPHONE_DUO: not in Apple's published API yet")
	contains(t, stderr, "11 files, the limit is 10")
}

func TestTextPushNeverTouchesMedia(t *testing.T) {
	e := setup(t, "PREPARE_FOR_SUBMISSION", "PREPARE_FOR_SUBMISSION")
	e.write("screenshots/en-US/APP_IPHONE_67/01.png", png(1320, 2868, false, ""))
	code, out, stderr := e.run("push", "--yes")
	must(t, code == 0, "exit %d: %s%s", code, out, stderr)
	for _, r := range e.fake.requests {
		must(t, !strings.Contains(r.Path, "Screenshot"), "text push touched %s", r.Path)
	}
}

func TestUnifiedDiff(t *testing.T) {
	got := strings.Join(unifiedDiff(strings.Split("a\nb\nc\nd\ne", "\n"), strings.Split("a\nb\nC\nd\ne", "\n")), "|")
	must(t, got == " b|-c|+C| d", "diff %q", got)
}

func TestInitWritesAConfigPerPlatform(t *testing.T) {
	dir := t.TempDir()
	old, _ := os.Getwd()
	os.Chdir(dir)
	t.Cleanup(func() { os.Chdir(old) })
	e := &env{t: t, repo: dir}

	code, out, _ := e.run("init", "com.example.app", "--platform", "mac_os")
	must(t, code == 0, "init failed: %d", code)
	contains(t, out, "appbird pull -c appbird.macos.json")
	var cfg map[string]string
	raw, _ := os.ReadFile(filepath.Join(dir, "appbird.macos.json"))
	json.Unmarshal(raw, &cfg)
	must(t, cfg["platform"] == "MAC_OS" && cfg["metadata_dir"] == "metadata-macos", "macOS config: %v", cfg)

	// With only a platform config, commands find it without -c.
	os.MkdirAll(filepath.Join(dir, "metadata-macos", "en-US"), 0o755)
	code, out, _ = e.run("validate")
	must(t, code == 0, "validate failed: %d", code)
	contains(t, out, "metadata-macos/ is valid")

	// Two platform configs and no appbird.json: ask for -c.
	e.run("init", "com.example.app", "--platform", "TV_OS")
	code, _, errOut := e.run("validate")
	must(t, code != 0, "expected validate to refuse two configs")
	contains(t, errOut, "Pick one with -c")

	// appbird.json (iOS) wins when it's there.
	code, _, _ = e.run("init", "com.example.app")
	must(t, code == 0, "iOS init failed: %d", code)
	raw, _ = os.ReadFile(filepath.Join(dir, "appbird.json"))
	json.Unmarshal(raw, &cfg)
	must(t, cfg["platform"] == "IOS" && cfg["metadata_dir"] == "metadata", "iOS config: %v", cfg)
	os.MkdirAll(filepath.Join(dir, "metadata", "en-US"), 0o755)
	code, out, _ = e.run("validate")
	must(t, code == 0, "validate failed: %d", code)
	contains(t, out, "metadata/ is valid")

	code, _, errOut = e.run("init", "com.example.app", "--platform", "WATCH_OS")
	must(t, code == 2, "expected an unknown platform to fail, got %d", code)
	contains(t, errOut, "Unknown platform")
}

// ---- TestFlight -----------------------------------------------------------------------------

func (e *env) query(path string) []string {
	var out []string
	for _, r := range e.fake.requests {
		if r.Method == "GET" && r.Path == path {
			out = append(out, r.Query.Encode())
		}
	}
	return out
}

func TestTestFlightPushesToTheNewestBuildOnALiveVersion(t *testing.T) {
	e := setup(t, "READY_FOR_DISTRIBUTION", "READY_FOR_DISTRIBUTION")
	e.write("testflight/en-US/what_to_test.txt", "Try the new map.\n")
	e.write("testflight/en-US/description.txt", "A journal for every visit.\n")
	e.write("testflight/de-DE/what_to_test.txt", "Teste die neue Karte.\n")
	e.write("testflight/de-DE/feedback_email.txt", "beta@example.de\n")
	code, out, _ := e.run("diff", "--exit-code")
	must(t, code == 2, "exit %d:\n%s", code, out)
	contains(t, out, "TestFlight build 1.1 (45)")
	contains(t, out, `testflight/en-US/what_to_test: "Old test notes" → "Try the new map."`)
	must(t, len(e.fake.writes()) == 0, "diff wrote %v", paths(e.fake.writes()))
	q := e.query("/v1/builds")
	must(t, len(q) == 1, "one build lookup: %v", q)
	for _, want := range []string{"filter%5Bapp%5D=app1", "filter%5BpreReleaseVersion.platform%5D=IOS",
		"filter%5Bexpired%5D=false", "sort=-uploadedDate", "limit=1"} {
		contains(t, q[0], want)
	}

	e.fake.reset()
	code, out, stderr := e.run("push", "--yes")
	must(t, code == 0, "exit %d: %s%s", code, out, stderr)
	got := paths(e.fake.writes())
	for _, want := range []string{"PATCH /v1/betaBuildLocalizations/bbl1", "POST /v1/betaBuildLocalizations",
		"PATCH /v1/betaAppLocalizations/bal1", "POST /v1/betaAppLocalizations"} {
		must(t, has(got, want), "missing %s in %v", want, got)
	}
	must(t, len(got) == 4, "only TestFlight writes: %v", got)
	must(t, e.fake.attr("betaBuildLocalizations", "bbl1", "whatsNew") == "Try the new map.", "what to test written")
	must(t, e.fake.attr("betaAppLocalizations", "bal1", "feedbackEmail") == "beta@example.com", "unmanaged field untouched")
	de := e.fake.children["/v1/builds/b1/betaBuildLocalizations"]
	must(t, len(de) == 2 && e.fake.attr("betaBuildLocalizations", de[1], "locale") == "de-DE", "de-DE added to the build: %v", de)
	for _, p := range got {
		must(t, !strings.Contains(p, "Submission"), "never submits for beta review: %s", p)
	}

	e.fake.reset()
	code, out, _ = e.run("diff", "--exit-code")
	must(t, code == 0, "second diff exit %d:\n%s", code, out)
}

func TestTestFlightBuildAndVersionFlags(t *testing.T) {
	e := setup(t, "PREPARE_FOR_SUBMISSION", "PREPARE_FOR_SUBMISSION")
	os.WriteFile(filepath.Join(e.repo, "appbird.json"),
		[]byte(`{"bundle_id": "com.example.app", "platform": "MAC_OS"}`), 0o644)
	e.write("testflight/en-US/what_to_test.txt", "Old test notes\n")
	code, _, stderr := e.run("diff", "--build", "45", "--app-version", "1.0")
	must(t, code == 0, "exit %d: %s", code, stderr)
	q := e.query("/v1/builds")
	must(t, len(q) == 1, "one build lookup: %v", q)
	contains(t, q[0], "filter%5Bversion%5D=45")
	contains(t, q[0], "filter%5BpreReleaseVersion.version%5D=1.0")
	contains(t, q[0], "filter%5BpreReleaseVersion.platform%5D=MAC_OS")
}

func TestTestFlightWithoutABuildWaits(t *testing.T) {
	e := setup(t, "PREPARE_FOR_SUBMISSION", "PREPARE_FOR_SUBMISSION")
	e.fake.children["/v1/builds"] = nil
	e.write("testflight/en-US/what_to_test.txt", "Try the new map.\n")
	e.write("testflight/en-US/description.txt", "New beta description\n")
	code, out, stderr := e.run("push", "--yes")
	must(t, code == 0, "exit %d: %s%s", code, out, stderr)
	contains(t, out, "can't be pushed now (no TestFlight build found")
	got := paths(e.fake.writes())
	must(t, len(got) == 1 && got[0] == "PATCH /v1/betaAppLocalizations/bal1", "only the app's test info: %v", got)
	code, _, _ = e.run("diff", "--exit-code")
	must(t, code == 2, "still different: %d", code)
}

func TestTestFlightValidation(t *testing.T) {
	e := setup(t, "PREPARE_FOR_SUBMISSION", "PREPARE_FOR_SUBMISSION")
	e.write("testflight/en-US/what_to_test.txt", strings.Repeat("x", 4001))
	e.write("testflight/en-US/feedback_email.txt", "not an email")
	e.write("testflight/en-US/privacy_url.txt", "example.com")
	e.write("testflight/en-US/notes.txt", "?")
	e.write("testflight/es-419/description.txt", "Hola")
	code, _, stderr := e.run("validate")
	must(t, code == 1, "exit %d", code)
	contains(t, stderr, "testflight/en-US/what_to_test: 4001 characters, the limit is 4000")
	contains(t, stderr, "testflight/en-US/feedback_email: not an email address")
	contains(t, stderr, "testflight/en-US/privacy_url: not a URL")
	contains(t, stderr, "ignored (not a field appbird knows): metadata/testflight/en-US/notes.txt")
	contains(t, stderr, "es-419: not a listing locale")
}

func TestStatusShowsTheNewestBuild(t *testing.T) {
	e := setup(t, "READY_FOR_DISTRIBUTION", "READY_FOR_DISTRIBUTION")
	code, out, stderr := e.run("status")
	must(t, code == 0, "exit %d: %s", code, stderr)
	contains(t, out, "TestFlight: newest build 1.1 (45), VALID")
}
