# Appbird

Keep your App Store listing in your repo. Appbird compares plain text files (and, if you want, your screenshots and artwork) with App Store Connect, shows you the difference, and writes it through the App Store Connect API when you say so.

```
$ appbird diff
Version 1.1 (PREPARE_FOR_SUBMISSION, editable)

update 1.1 en-US
  ~ en-US/promotional_text: 'Rate it in one tap.' → 'Rate it in one tap, now in German.'
  ~ en-US/description
      @@ -3,1 +3,1 @@
      -Every visit, kept whole
      +Every journal entry, kept whole

$ appbird push
Write 2 change(s) to App Store Connect? [y/N] y
→ update 1.1 en-US
Done: 2 change(s) written. Nothing was submitted for review.
```

- **One binary, no dependencies.** A single static executable for macOS (Apple silicon and Intel), Linux and Windows. No Ruby, no gems, no Python, no OpenSSL.
- **Safe by default.** `diff` only reads. `push` shows the diff and asks first. Appbird never submits anything for review, never releases a version, never deletes a localization, and never touches prices, availability, or your App Review contact details.
- **fastlane-compatible layout.** The text files use fastlane deliver's names (`metadata/en-US/description.txt`), so you can move between the two.
- **Works for any app.** One small `appbird.json` per app repo, and one per platform if it ships on iOS, macOS, tvOS or visionOS.
- **Easy to hand to an AI agent.** Copy-paste prompts below [set it up for you](#set-it-up-with-an-ai-agent).

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/TweetingBirds/appbird/main/install.sh | sh
```

That downloads the right binary from the [latest release](https://github.com/TweetingBirds/appbird/releases), checks its SHA-256, and puts it in `/usr/local/bin` (`APPBIRD_DIR=~/bin` to change that, `APPBIRD_VERSION=0.1.0` to pin one). You can also download a release by hand, or build it with Go 1.22 or later:

```sh
go install github.com/TweetingBirds/appbird@latest
```

A binary downloaded with a browser on macOS is quarantined by Gatekeeper. `xattr -d com.apple.quarantine appbird` clears that. `curl` and `go install` don't set the flag.

## Set up an app

```sh
cd my-app
appbird init com.example.myapp     # writes appbird.json (add --platform MAC_OS for a Mac app)
export ASC_KEY_ID=… ASC_ISSUER_ID=… ASC_PRIVATE_KEY_PATH=~/.appstoreconnect/private_keys/AuthKey_….p8
appbird status                     # versions, and which one is editable
appbird pull                       # writes metadata/ from what's in App Store Connect now
git add appbird.json metadata && git commit -m "Track App Store metadata"
```

From then on, edit the files, open a pull request, run `appbird diff`, and `appbird push` once it's merged.

`appbird.json`:

```json
{
  "bundle_id": "com.example.myapp",
  "platform": "IOS",
  "metadata_dir": "metadata"
}
```

`platform` is `IOS` (the default), `MAC_OS`, `TV_OS` or `VISION_OS`. If your app ships on more than one, see [Apps on more than one platform](#apps-on-more-than-one-platform).

## Apps on more than one platform

One config file manages one platform's App Store versions. Appbird asks App Store Connect only for versions of the `platform` it names, so `status`, `diff`, `push`, `pull`, `media` and `--app-version` all see that platform alone. The `--platform` flag only matters to `appbird init`. Every other command reads the platform from the config.

Apple Watch apps and iMessage apps aren't platforms of their own here. They're part of the iOS version, so they use `IOS`, and their screenshots go in the `APP_WATCH_…` and `IMESSAGE_APP_…` folders next to the iPhone and iPad ones.

`appbird init` names the config after the platform, so each platform gets its own config and metadata folder:

| `--platform` | Config | Metadata folder |
|---|---|---|
| `IOS` (default) | `appbird.json` | `metadata/` |
| `MAC_OS` | `appbird.macos.json` | `metadata-macos/` |
| `TV_OS` | `appbird.tvos.json` | `metadata-tvos/` |
| `VISION_OS` | `appbird.visionos.json` | `metadata-visionos/` |

Commands look for `appbird.json` first. Without one, they use the only `appbird.<platform>.json` in the folder, so a Mac-only app never needs `-c`. When there's more than one config (for example, an app record in App Store Connect with both an iOS and a macOS version, a universal purchase), pick a platform other than the one in `appbird.json` with `-c`:

```
appbird.json               {"bundle_id": "com.example.myapp", "platform": "IOS",    "metadata_dir": "metadata"}
appbird.macos.json         {"bundle_id": "com.example.myapp", "platform": "MAC_OS", "metadata_dir": "metadata-macos"}
metadata/
metadata-macos/
```

```sh
appbird init com.example.myapp                          # writes appbird.json
appbird init com.example.myapp --platform MAC_OS        # writes appbird.macos.json
appbird pull                                            # iOS
appbird pull -c appbird.macos.json                      # macOS
appbird diff -c appbird.macos.json
appbird media diff -c appbird.macos.json --app-version 2.0
```

**What each platform has of its own, and what they share.** Some of the listing belongs to each platform's version. The rest belongs to the app and is the same on every platform:

| Per platform (one copy in each metadata folder) | Shared by the whole app |
|---|---|
| `description`, `keywords`, `promotional_text`, `release_notes`, `support_url`, `marketing_url` | `name`, `subtitle`, `privacy_url`, `privacy_choices_url` (App Info) |
| `copyright.txt`, `review_information/notes.txt` | `eula.txt` |
| `screenshots/`, `previews/`, and where creative assets are placed | `subscriptions/`, `subscription_groups/`, and the Asset Library that creative assets are uploaded to |

Keep each shared file in **one** metadata folder only. `pull` writes every file into whichever folder you pulled into, so after pulling the second platform, delete its copies of the shared files (`name.txt`, `subtitle.txt`, the privacy URLs, `eula.txt`, `subscriptions/`, `subscription_groups/`). A missing file isn't managed, so the other config keeps owning them. If both folders keep a shared file and the two copies differ, each `push` overwrites the other's text.

Things Appbird doesn't check for you:

- **The platform value in a config you wrote by hand.** `appbird init` refuses an unknown platform, but a typo such as `MACOS` typed into a config later isn't caught offline. `status` then lists no versions, and `diff` reports that the app has no App Store version yet.
- **Display types against the platform.** `media validate` accepts every display type Apple has, whatever the platform. Use the ones for that platform: `APP_DESKTOP` and `DESKTOP` for macOS, `APP_APPLE_TV` and `APPLE_TV` for tvOS, `APP_APPLE_VISION_PRO` and `APPLE_VISION_PRO` for visionOS, and the iPhone, iPad, Watch and iMessage ones for iOS.

In CI, run each step once per config (`appbird diff`, then `appbird diff -c appbird.macos.json`), and add the second metadata folder (such as `metadata-macos/**`) to the workflow's `pull_request` paths filter. The filter already matches every `appbird*.json`.

## API key

1. In [App Store Connect › Users and Access › Integrations › App Store Connect API](https://appstoreconnect.apple.com/access/integrations/api), generate a **Team key** with the **App Manager** role (Admin works too).
2. Download the `.p8` file. Apple lets you download it **once**. Keep it in a password manager or `~/.appstoreconnect/private_keys/`, never in a repo.
3. Note the **Key ID** and the **Issuer ID** shown on that page.

Appbird reads:

| Variable | Value |
|---|---|
| `ASC_KEY_ID` | the Key ID |
| `ASC_ISSUER_ID` | the Issuer ID (leave unset for an *individual* key) |
| `ASC_PRIVATE_KEY_PATH` | path to the `.p8` file, **or** |
| `ASC_PRIVATE_KEY` | the file's contents (for CI secrets; `\n` escapes are fine) |

Without either key variable, Appbird looks for `AuthKey_<KEY_ID>.p8` in `./private_keys`, `~/private_keys`, `~/.private_keys` and `~/.appstoreconnect/private_keys`, the same places Apple's `altool` uses.

## Set it up with an AI agent

If you use an AI coding agent (Claude Code, Codex, Cursor, Copilot and the like), you can paste one of these prompts into it from your app's repo. Each one tells the agent to stop and ask before anything that writes to App Store Connect, and to keep your API key out of the repo and out of the chat.

The agent can't create the API key for you. Do the three [API key](#api-key) steps yourself first, and export the variables in the shell the agent runs in (or put the `.p8` file in `~/.appstoreconnect/private_keys/`).

**Install Appbird and start tracking this app's metadata**

```text
Set up Appbird (https://github.com/TweetingBirds/appbird) in this repo so the App Store
listing lives in files here. Read its README first and follow it.

1. Install the binary if `appbird version` doesn't work:
   curl -fsSL https://raw.githubusercontent.com/TweetingBirds/appbird/main/install.sh | sh
   If /usr/local/bin isn't writable without sudo, use APPBIRD_DIR=$HOME/.local/bin and
   make sure that folder is on PATH.
2. Find the app's bundle ID and platform from the Xcode project (PRODUCT_BUNDLE_IDENTIFIER
   and SDKROOT) and confirm them with me. Platform is IOS, MAC_OS, TV_OS or VISION_OS.
   Then run: appbird init <bundle id> --platform <platform>
3. Check that ASC_KEY_ID, ASC_ISSUER_ID and ASC_PRIVATE_KEY_PATH (or ASC_PRIVATE_KEY) are
   set. Don't print their values, and never ask me to paste the key into the chat. If
   they're missing, tell me and wait.
4. Run `appbird status`, then `appbird pull` to write the metadata folder from App Store
   Connect, then `appbird validate`. Show me the output.
5. Make sure no .p8 file is inside the repo, then commit the config and metadata folder that
   init wrote (appbird.json and metadata/ for iOS, appbird.macos.json and metadata-macos/
   for macOS, and so on) on a new branch.

Don't run `appbird push` or `appbird media push`. I'll do that myself.
```

**Add the GitHub Actions workflow**

```text
Add Appbird's CI workflow to this repo. Copy
https://raw.githubusercontent.com/TweetingBirds/appbird/main/examples/app-store-metadata.yml
to .github/workflows/app-store-metadata.yml. Set APPBIRD_VERSION in it to the latest
release from https://github.com/TweetingBirds/appbird/releases. If the repo's Appbird
config (appbird.json, or appbird.<platform>.json for a non-iOS app) uses a metadata_dir
other than "metadata", update the pull_request paths filter to match. If the repo has more
than one config, run each appbird step once per config with -c.
Then tell me to add ASC_KEY_ID, ASC_ISSUER_ID and ASC_PRIVATE_KEY (the whole .p8 file) as
repository secrets under Settings › Secrets and variables › Actions. Don't add the secrets
or the key file yourself. Open a pull request with the change.
```

**Move from fastlane deliver**

```text
This repo uses fastlane deliver for App Store metadata. Set up Appbird
(https://github.com/TweetingBirds/appbird) to read the same text files: install it as
its README says, run `appbird init <bundle id> --platform <platform>` with the app's
bundle ID, and set "metadata_dir" in the config it wrote to the existing fastlane metadata
folder (usually fastlane/metadata). Run `appbird validate` and tell me what it reports;
fastlane-only files (such as primary_category.txt or a default/ folder) may show up there.
Then run `appbird diff` and show me what differs from App Store Connect.
Don't move or delete any fastlane files, and don't push. Screenshots use a different
folder layout in Appbird (see the Media section of its README), so leave them for now.
```

**Edit the listing**

```text
Update this app's App Store listing in metadata/ (Appbird, see appbird.json):
<what you want: new release notes for 1.3, a German localization, sharper keywords, ...>
Follow the file rules in https://github.com/TweetingBirds/appbird#files: one field per
file, and respect the character limits (name and subtitle 30, keywords 100, promotional
text 170, description and release notes 4000). Run `appbird validate` and fix anything
it reports, then run `appbird diff` and show me the result. Commit on a branch and open a
pull request. Don't run `appbird push`.
```

## Commands

| Command | What it does |
|---|---|
| `appbird init <bundle id> [--platform X]` | Write `appbird.json` here, or `appbird.<platform>.json` for a platform other than iOS. |
| `appbird validate` | Check the files offline: character limits, URLs, locale codes, stray files. No key needed. |
| `appbird status` | List the app's versions and their states. |
| `appbird diff [--exit-code]` | Show what differs. Read-only. `--exit-code` exits 2 when something differs. |
| `appbird push [--yes]` | Show the diff, ask, then write it. `--yes` skips the question (needed off a terminal). |
| `appbird pull [--locale xx]` | Overwrite the repo's files with App Store Connect's text. Review with `git diff`. |
| `appbird media validate\|diff\|push` | The same for screenshots, app previews and creative assets. See [Media](#media). |
| `appbird version` | Print the version. |

`diff`, `push`, `pull` and `media` take `--app-version 1.2` to pick a version. By default Appbird uses the version you're preparing, or the live one if there isn't one.

**What can change when.** Apple only accepts most edits while a version is being prepared (or after it was rejected). With only a live version, `push` writes the promotional text, subscription copy and the EULA, and lists everything else as waiting. Create the next version in App Store Connect and push again.

## Files

```
appbird.json
metadata/
├── copyright.txt                      © 2026 Example LLC
├── eula.txt                           custom EULA text (only updates one that exists)
├── review_information/
│   └── notes.txt                      App Review notes
├── en-US/
│   ├── name.txt                       30 characters
│   ├── subtitle.txt                   30
│   ├── privacy_url.txt
│   ├── privacy_choices_url.txt
│   ├── description.txt                4000
│   ├── keywords.txt                   100, comma separated
│   ├── promotional_text.txt           170, can change any time
│   ├── release_notes.txt              "What's New", 4000
│   ├── support_url.txt
│   └── marketing_url.txt
├── de-DE/ …
├── subscriptions/
│   └── com.example.premium.annual/
│       ├── review_note.txt
│       └── en-US/
│           ├── name.txt               35
│           └── description.txt        55
├── subscription_groups/
│   └── Premium/                       the group's reference name
│       └── en-US/
│           ├── name.txt
│           └── custom_app_name.txt
├── screenshots/ previews/ creative_assets/   see Media
```

Rules:

- **A missing file isn't managed.** Appbird leaves that field alone in App Store Connect.
- **An empty file clears the field** (for fields Apple lets you clear, such as the marketing URL).
- **A locale folder that App Store Connect doesn't have yet is added** when you push. A new locale needs at least `name.txt`.
- **Locales in App Store Connect but not in the repo are left alone.** Appbird never deletes one.
- Trailing whitespace and the final newline are ignored, so editors can't create false diffs.
- Folder names are App Store Connect's listing locales: `en-US`, `de-DE`, `fr-FR`, `it`, `ja`, `pt-BR`, `es-ES`, `es-MX` (Latin America), `zh-Hant`, and so on.
- Review contact details (name, phone, email) and demo account credentials stay in App Store Connect on purpose, so they never land in a repo.

## Media

Screenshots, app preview videos and creative assets (the product page header and search results art introduced with iOS 27) live in their own folders and only move with `appbird media`. A text `push` never touches them.

```
metadata/
├── screenshots/
│   └── en-US/
│       ├── APP_IPHONE_67/            one folder per display type
│       │   ├── 01-journal.png        uploaded in file name order, up to 10
│       │   └── 02-map.png
│       ├── APP_IPAD_PRO_3GEN_129/
│       └── APP_IPHONE_DUO/
├── previews/
│   └── en-US/
│       └── IPHONE_67/
│           └── 01-tour.mov           up to 3
└── creative_assets/
    ├── header.png                    every locale (3840×1646 or 5244×2950, no alpha)
    ├── search_results.png            every locale (3:2, or 5244×2950)
    └── de-DE/
        └── search_results.png        overrides the default for one locale
```

How syncing works:

- **Screenshots and previews** are matched by MD5 checksum (Apple keeps one per file). An unchanged file is never uploaded again, even if you rename it. When a set differs, Appbird uploads the new files, removes the ones you deleted, and puts the set in your file order. A display type folder you don't have is left alone. Apple only accepts these on a version you're preparing.
- **Creative assets** go into the app's Asset Library once (named `appbird-<md5>`) and are placed on each locale's product page. A changed file gets a new placement, and the old placement is removed. Apple reviews creative assets on their own. Appbird places them but doesn't submit them, so submit them from App Store Connect › Asset Library.
- Folder names are Apple's API values (`APP_IPHONE_67` is the 6.9" and 6.7" iPhone, `APP_IPAD_PRO_3GEN_129` the 13" iPad). iPhone Duo (`APP_IPHONE_DUO`, `IPHONE_DUO`) isn't in Apple's published API reference yet, but the live API is reported to accept it, so Appbird lets you try it with a warning.
- `appbird media diff` shows exactly what would be uploaded and removed. Run it before every `media push`.
- There is no media `pull` yet.

## In CI

[`examples/app-store-metadata.yml`](examples/app-store-metadata.yml) validates and diffs every pull request that touches `metadata/`, and pushes only when you run it by hand. Add `ASC_KEY_ID`, `ASC_ISSUER_ID` and `ASC_PRIVATE_KEY` as repository secrets. On a public repo, a pull request from a fork can't read secrets, so it only gets the offline validation.

## Development

```sh
go test ./...
```

The tests run the whole CLI against a fake App Store Connect server and a throwaway key, so they need no network and no Apple account. To release, push a tag such as `v0.1.0`. The release workflow builds the binaries and publishes them with checksums. Endpoints follow Apple's [OpenAPI specification](https://developer.apple.com/sample-code/app-store-connect/app-store-connect-openapi-specification.zip) (4.6).

## License

MIT. Made by [Tweeting Birds](https://tweetingbirds.com).
