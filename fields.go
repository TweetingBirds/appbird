package main

import "regexp"

// field maps a metadata file name (without .txt) to an App Store Connect attribute.
type field struct {
	File  string
	API   string
	Limit int // characters; 0 means no limit
}

// Per-locale fields on the app's App Info, shared by every version.
var appInfoFields = []field{
	{"name", "name", 30},
	{"subtitle", "subtitle", 30},
	{"privacy_url", "privacyPolicyUrl", 0},
	{"privacy_choices_url", "privacyChoicesUrl", 0},
}

// Per-locale fields on one App Store version.
var versionFields = []field{
	{"description", "description", 4000},
	{"keywords", "keywords", 100},
	{"promotional_text", "promotionalText", 170},
	{"release_notes", "whatsNew", 4000},
	{"support_url", "supportUrl", 0},
	{"marketing_url", "marketingUrl", 0},
}

var subscriptionLocFields = []field{
	{"name", "name", 35},
	{"description", "description", 55},
}

// In-app purchases (consumable, non-consumable, non-renewing subscription) use the same
// limits as subscriptions.
var inAppPurchaseLocFields = subscriptionLocFields

var subscriptionGroupLocFields = []field{
	{"name", "name", 75},
	{"custom_app_name", "customAppName", 30},
}

// Promotional text is the one version field Apple lets you change on a live version.
var liveEditableVersionFields = map[string]bool{"promotional_text": true}

var urlFields = map[string]bool{
	"privacy_url": true, "privacy_choices_url": true, "support_url": true, "marketing_url": true,
}

var oneLineFields = map[string]bool{"name": true, "subtitle": true, "custom_app_name": true}

const reviewNotesLimit = 4000
const subscriptionReviewNoteLimit = 4000
const inAppPurchaseReviewNoteLimit = 4000

// Folders inside metadata/ that aren't locales.
var specialDirs = map[string]bool{
	"review_information": true, "subscriptions": true, "subscription_groups": true, "in_app_purchases": true,
	"screenshots": true, "previews": true, "creative_assets": true,
}

// App Store Connect's listing locales. Unknown codes only warn, so new ones keep working.
var knownLocales = setOf(
	"ar-SA", "bn-BD", "ca", "cs", "da", "de-DE", "el", "en-AU", "en-CA", "en-GB", "en-US",
	"es-ES", "es-MX", "fi", "fr-CA", "fr-FR", "gu-IN", "he", "hi", "hr", "hu", "id", "it",
	"ja", "kn-IN", "ko", "ml-IN", "mr-IN", "ms", "nl-NL", "no", "or-IN", "pa-IN", "pl",
	"pt-BR", "pt-PT", "ro", "ru", "sk", "sl-SI", "sv", "ta-IN", "te-IN", "th", "tr", "uk",
	"ur-PK", "vi", "zh-Hans", "zh-Hant",
)

var localeRE = regexp.MustCompile(`^[a-z]{2,3}(-[A-Za-z0-9]{2,4})?$`)

// Version states in which Apple accepts edits to a version's metadata.
var editableVersionStates = setOf(
	"PREPARE_FOR_SUBMISSION", "DEVELOPER_REJECTED", "REJECTED", "METADATA_REJECTED",
	"INVALID_BINARY", "READY_FOR_REVIEW",
)
var liveVersionStates = setOf("READY_FOR_DISTRIBUTION", "READY_FOR_SALE")
var editableAppInfoStates = setOf(
	"PREPARE_FOR_SUBMISSION", "DEVELOPER_REJECTED", "REJECTED", "METADATA_REJECTED", "READY_FOR_REVIEW",
)

func setOf(items ...string) map[string]bool {
	m := make(map[string]bool, len(items))
	for _, i := range items {
		m[i] = true
	}
	return m
}

func fieldNames(fs []field) map[string]bool {
	m := map[string]bool{}
	for _, f := range fs {
		m[f.File] = true
	}
	return m
}
