// Appbird keeps an app's App Store Connect listing in sync with files in its repo.
//
// It only ever edits listing text and media. It never submits anything for review, releases a
// version, deletes a localization, or touches prices or availability. See README.md.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
)

// version is set at build time with -ldflags "-X main.version=..."
var version = "dev"

const usage = `appbird: keep your App Store Connect listing in sync with files in your repo.
Never submits anything for review.

Usage:
  appbird init <bundle id> [--platform IOS]   write appbird.json here (appbird.macos.json
                                              for MAC_OS, and so on)
  appbird validate                            check metadata/ offline (limits, URLs, locales)
  appbird status                              list the app's versions and which one is editable
  appbird diff  [--app-version X.Y] [--exit-code]
                                              show what differs (read-only)
  appbird push  [--app-version X.Y] [--yes]   write the repo's text to App Store Connect
  appbird pull  [--app-version X.Y] [--locale xx]
                                              write App Store Connect's text into metadata/
  appbird media validate|diff|push|pull [--only screenshots|previews|creative] [--app-version X.Y]
                [--exit-code] [--yes] [--locale xx]
                                              screenshots, app previews and creative assets
                                              (pull: screenshots and previews only)
  appbird create-version <X.Y.Z> [--yes]     create the next App Store version to edit
  appbird version

Every command takes -c <path to appbird.json> and -v (print each API request).
API key: ASC_KEY_ID, ASC_ISSUER_ID, and ASC_PRIVATE_KEY_PATH or ASC_PRIVATE_KEY.
`

type options struct {
	config, appVersion, platform string
	verbose, yes, exitCode       bool
	locales, only                multiFlag
	args                         []string
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func parseOptions(name string, args []string, stderr io.Writer) (*options, error) {
	o := &options{}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, usage) }
	fs.StringVar(&o.config, "c", "", "")
	fs.StringVar(&o.config, "config", "", "")
	fs.BoolVar(&o.verbose, "v", false, "")
	fs.BoolVar(&o.verbose, "verbose", false, "")
	fs.StringVar(&o.appVersion, "app-version", "", "")
	fs.StringVar(&o.platform, "platform", "IOS", "")
	fs.BoolVar(&o.yes, "y", false, "")
	fs.BoolVar(&o.yes, "yes", false, "")
	fs.BoolVar(&o.exitCode, "exit-code", false, "")
	fs.Var(&o.locales, "locale", "")
	fs.Var(&o.only, "only", "")
	// Allow flags before and after positional arguments.
	rest := args
	for {
		if err := fs.Parse(rest); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			break
		}
		o.args = append(o.args, fs.Arg(0))
		rest = fs.Args()[1:]
	}
	return o, nil
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// app holds a command's streams, so tests can run the CLI in-process.
type app struct {
	in       io.Reader
	out, err io.Writer
	isTTY    bool
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprint(stdout, usage)
		return 0
	}
	a := &app{in: stdin, out: stdout, err: stderr, isTTY: isTerminal(stdin)}
	progress = stdout
	cmd := args[0]
	o, err := parseOptions(cmd, args[1:], stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	var code int
	switch cmd {
	case "version", "--version":
		fmt.Fprintln(stdout, "appbird", version)
		return 0
	case "init":
		code, err = a.cmdInit(o)
	case "validate":
		code, err = a.cmdValidate(o)
	case "status":
		code, err = a.cmdStatus(o)
	case "diff":
		code, err = a.cmdDiffPush(o, false)
	case "push":
		code, err = a.cmdDiffPush(o, true)
	case "pull":
		code, err = a.cmdPull(o)
	case "media":
		code, err = a.cmdMedia(o)
	case "create-version":
		code, err = a.cmdCreateVersion(o)
	default:
		fmt.Fprintf(stderr, "appbird: unknown command %q\n\n%s", cmd, usage)
		return 2
	}
	if err != nil {
		fmt.Fprintf(stderr, "appbird: %v\n", err)
		if code == 0 {
			code = 1
		}
	}
	return code
}

func isTerminal(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func (a *app) cmdInit(o *options) (int, error) {
	if len(o.args) != 1 {
		return 2, userErr("Pass the app's bundle ID: appbird init com.example.app")
	}
	platform := strings.ToUpper(o.platform)
	if !platforms[platform] {
		return 2, userErr("Unknown platform %q. Use IOS, MAC_OS, TV_OS or VISION_OS.", o.platform)
	}
	path, dir := configFor(platform)
	if _, err := os.Stat(path); err == nil {
		return 1, userErr("%s already exists.", path)
	}
	raw, _ := json.MarshalIndent(map[string]string{"bundle_id": o.args[0], "platform": platform, "metadata_dir": dir}, "", "  ")
	if err := os.WriteFile(path, append(raw, '\n'), 0o644); err != nil {
		return 1, err
	}
	next := "run `appbird pull`"
	if path != configName {
		next = "run `appbird pull -c " + path + "`"
	}
	fmt.Fprintf(a.out, "Wrote %s. Next: set the API key variables and %s.\n", path, next)
	return 0, nil
}

func (a *app) report(problems, warnings []string, what string) error {
	for _, w := range warnings {
		fmt.Fprintln(a.err, "warning:", w)
	}
	for _, p := range problems {
		fmt.Fprintln(a.err, "error:", p)
	}
	if len(problems) > 0 {
		return userErr("%d problem(s) in %s; nothing was sent.", len(problems), what)
	}
	return nil
}

func (a *app) localChecked(cfg Config) (*LocalMetadata, error) {
	m, err := loadLocal(cfg)
	if err != nil {
		return nil, err
	}
	problems, warnings := validateLocal(cfg, m)
	return m, a.report(problems, warnings, cfg.MetadataDir+"/")
}

func (a *app) client(o *options) (*Client, error) {
	creds, err := loadCredentials()
	if err != nil {
		return nil, err
	}
	return newClient(creds, o.verbose), nil
}

func (a *app) cmdValidate(o *options) (int, error) {
	cfg, err := findConfig(".", o.config)
	if err != nil {
		return 1, err
	}
	m, err := a.localChecked(cfg)
	if err != nil {
		return 1, err
	}
	locales := sortedKeys(m.AppInfo)
	fmt.Fprintf(a.out, "%s/ is valid: %d locale(s) (%s), %d subscription(s), %d in-app purchase(s).\n", cfg.MetadataDir, len(locales),
		firstNonEmpty(strings.Join(locales, ", "), "none"), len(m.Subscriptions), len(m.InAppPurchases))
	return 0, nil
}

func (a *app) cmdStatus(o *options) (int, error) {
	cfg, err := findConfig(".", o.config)
	if err != nil {
		return 1, err
	}
	c, err := a.client(o)
	if err != nil {
		return 1, err
	}
	r, err := fetchRemote(c, cfg, "", false)
	if err != nil {
		return 1, err
	}
	fmt.Fprintf(a.out, "%s (%s), app ID %s\n", r.App.Str("name"), cfg.BundleID, r.App.ID)
	for _, v := range r.Versions {
		state, tag := versionState(v), ""
		if editableVersionStates[state] {
			tag = " ← editable"
		} else if liveVersionStates[state] {
			tag = " ← live"
		}
		fmt.Fprintf(a.out, "  %8s  %s%s\n", v.Str("versionString"), state, tag)
	}
	fmt.Fprintln(a.out, "Locales:", strings.Join(sortedKeys(r.VersionLocs), ", "))
	return 0, nil
}

// show prints the plan and mirrors it into the GitHub Actions job summary, if there is one.
func (a *app) show(p *Plan, r *Remote) {
	var buf bytes.Buffer
	printPlan(&buf, p, r)
	text := strings.TrimRight(buf.String(), "\n")
	fmt.Fprintln(a.out, text)
	if path := os.Getenv("GITHUB_STEP_SUMMARY"); path != "" {
		if f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			fmt.Fprintf(f, "```\n%s\n```\n", text)
			f.Close()
		}
	}
}

func (a *app) confirm(question string, yes bool) (bool, error) {
	if yes {
		return true, nil
	}
	if !a.isTTY {
		return false, userErr("Not a terminal: pass --yes to push.")
	}
	fmt.Fprintf(a.out, "\n%s [y/N] ", question)
	line, _ := bufio.NewReader(a.in).ReadString('\n')
	answer := strings.ToLower(strings.TrimSpace(line))
	if answer == "y" || answer == "yes" {
		return true, nil
	}
	fmt.Fprintln(a.out, "Nothing was sent.")
	return false, nil
}

// execute runs the plan in order and stops at the first failure. Every operation is computed
// from App Store Connect's current state, so running the same command again picks up where
// this one stopped.
func (a *app) execute(c *Client, p *Plan) error {
	for i, op := range p.Operations {
		fmt.Fprintln(a.out, "→", op.Describe)
		err := op.Run(c)
		if err == nil {
			continue
		}
		fmt.Fprintf(a.err, "\nStopped at step %d of %d: %s\n  %v\n", i+1, len(p.Operations), op.Describe, err)
		if i > 0 {
			fmt.Fprintf(a.err, "Written before it stopped (%d):\n", i)
			for _, done := range p.Operations[:i] {
				fmt.Fprintln(a.err, "  ✓", done.Describe)
			}
		}
		fmt.Fprintf(a.err, "Not written (%d):\n", len(p.Operations)-i)
		for _, rest := range p.Operations[i:] {
			fmt.Fprintln(a.err, "  ✗", rest.Describe)
		}
		var uncertain uncertainError
		if errors.As(err, &uncertain) {
			fmt.Fprintln(a.err, "The failed step may or may not have reached App Store Connect.")
		}
		return userErr("%d of %d step(s) written. Run the diff again to see what's left; pushing again continues from there.", i, len(p.Operations))
	}
	return nil
}

func (a *app) cmdDiffPush(o *options, push bool) (int, error) {
	cfg, err := findConfig(".", o.config)
	if err != nil {
		return 1, err
	}
	m, err := a.localChecked(cfg)
	if err != nil {
		return 1, err
	}
	c, err := a.client(o)
	if err != nil {
		return 1, err
	}
	r, err := fetchRemote(c, cfg, o.appVersion, true)
	if err != nil {
		return 1, err
	}
	p := buildPlan(m, r)
	a.show(p, r)
	if !push {
		if o.exitCode && (len(p.Operations) > 0 || len(p.Locked) > 0) {
			return 2, nil
		}
		return 0, nil
	}
	if len(p.Operations) == 0 {
		fmt.Fprintln(a.out, "\nNothing to push.")
		return 0, nil
	}
	ok, err := a.confirm(fmt.Sprintf("Write %d change(s) to App Store Connect?", p.ChangeCount()), o.yes)
	if err != nil || !ok {
		return 1, err
	}
	if err := a.execute(c, p); err != nil {
		return 1, err
	}
	fmt.Fprintf(a.out, "Done: %d change(s) written. Nothing was submitted for review.\n", p.ChangeCount())
	if len(p.Locked) > 0 {
		fmt.Fprintf(a.out, "%d change(s) are waiting for an editable version.\n", len(p.Locked))
	}
	return 0, nil
}

var versionStringRE = regexp.MustCompile(`^\d+(\.\d+){0,2}$`)

// cmdCreateVersion creates a new App Store version. Apple copies the previous version's text
// into it. Nothing is submitted.
func (a *app) cmdCreateVersion(o *options) (int, error) {
	if len(o.args) != 1 || !versionStringRE.MatchString(o.args[0]) {
		return 2, userErr("Pass the new version number: appbird create-version 1.2")
	}
	want := o.args[0]
	cfg, err := findConfig(".", o.config)
	if err != nil {
		return 1, err
	}
	c, err := a.client(o)
	if err != nil {
		return 1, err
	}
	r, err := fetchRemote(c, cfg, "", false)
	if err != nil {
		return 1, err
	}
	for _, v := range r.Versions {
		if v.Str("versionString") == want {
			return 1, userErr("%s version %s already exists (%s).", cfg.Platform, want, versionState(v))
		}
		if editableVersionStates[versionState(v)] {
			return 1, userErr("Version %s is already being prepared (%s). Apple allows one at a time: edit that one, or change its number in App Store Connect.",
				v.Str("versionString"), versionState(v))
		}
	}
	ok, err := a.confirm(fmt.Sprintf("Create %s version %s of %s in App Store Connect?", cfg.Platform, want, r.App.Str("name")), o.yes)
	if err != nil || !ok {
		return 1, err
	}
	v, err := c.Create("appStoreVersions", map[string]any{"platform": cfg.Platform, "versionString": want},
		map[string]any{"app": rel("apps", r.App.ID)})
	var uncertain uncertainError
	if errors.As(err, &uncertain) {
		return 1, userErr("%v\nThe version may or may not have been created. Run `appbird status` before trying again.", err)
	}
	if err != nil {
		return 1, err
	}
	fmt.Fprintf(a.out, "Created version %s (%s). Apple copies the text from the previous version.\n", want, firstNonEmpty(versionState(v), "PREPARE_FOR_SUBMISSION"))
	fmt.Fprintln(a.out, "Next: `appbird diff` to see what the repo would change. Nothing was submitted for review.")
	return 0, nil
}

func (a *app) cmdPull(o *options) (int, error) {
	cfg, err := findConfig(".", o.config)
	if err != nil {
		return 1, err
	}
	c, err := a.client(o)
	if err != nil {
		return 1, err
	}
	r, err := fetchRemote(c, cfg, o.appVersion, true)
	if err != nil {
		return 1, err
	}
	only := map[string]bool{}
	for _, l := range o.locales {
		only[l] = true
	}
	written, err := pull(cfg, r, only)
	if err != nil {
		return 1, err
	}
	v := firstNonEmpty(r.VersionString(), "none")
	if len(written) == 0 {
		fmt.Fprintf(a.out, "%s/ already matches App Store Connect (version %s).\n", cfg.MetadataDir, v)
		return 0, nil
	}
	fmt.Fprintf(a.out, "Wrote %d file(s) from version %s:\n", len(written), v)
	for _, w := range written {
		fmt.Fprintln(a.out, " ", cfg.Rel(w))
	}
	fmt.Fprintln(a.out, "Review with `git diff` before committing.")
	return 0, nil
}

func (a *app) cmdMediaPull(o *options, cfg Config) (int, error) {
	kinds := map[string]bool{"screenshots": true, "previews": true}
	if len(o.only) > 0 {
		kinds = map[string]bool{}
		for _, k := range o.only {
			if k != "screenshots" && k != "previews" {
				return 2, userErr("media pull takes --only screenshots or previews (creative assets can't be pulled yet)")
			}
			kinds[k] = true
		}
	}
	c, err := a.client(o)
	if err != nil {
		return 1, err
	}
	r, err := fetchRemote(c, cfg, o.appVersion, false)
	if err != nil {
		return 1, err
	}
	res, err := pullMedia(c, cfg, r, kinds, setOf(o.locales...))
	if err != nil {
		return 1, err
	}
	for _, n := range res.Notes {
		fmt.Fprintln(a.out, "note:", n)
	}
	v := firstNonEmpty(r.VersionString(), "none")
	if len(res.Written)+len(res.Removed) == 0 {
		fmt.Fprintf(a.out, "%s/ media already matches App Store Connect (version %s).\n", cfg.MetadataDir, v)
		return 0, nil
	}
	fmt.Fprintf(a.out, "Wrote %d file(s) and removed %d from version %s:\n", len(res.Written), len(res.Removed), v)
	for _, w := range res.Written {
		fmt.Fprintln(a.out, "  +", cfg.Rel(w))
	}
	for _, w := range res.Removed {
		fmt.Fprintln(a.out, "  -", cfg.Rel(w))
	}
	fmt.Fprintln(a.out, "Apple re-encodes media, so these aren't your original files. Review with `git status` before committing.")
	return 0, nil
}

func (a *app) cmdMedia(o *options) (int, error) {
	if len(o.args) != 1 || !setOf("validate", "diff", "push", "pull")[o.args[0]] {
		return 2, userErr("Usage: appbird media validate|diff|push|pull")
	}
	action := o.args[0]
	cfg, err := findConfig(".", o.config)
	if err != nil {
		return 1, err
	}
	if action == "pull" {
		return a.cmdMediaPull(o, cfg)
	}
	m := loadMedia(cfg)
	if len(o.only) > 0 {
		keep := setOf(o.only...)
		for k := range keep {
			if k != "screenshots" && k != "previews" && k != "creative" {
				return 2, userErr("--only takes screenshots, previews or creative")
			}
		}
		if !keep["screenshots"] {
			m.Screenshots = map[string]map[string][]string{}
		}
		if !keep["previews"] {
			m.Previews = map[string]map[string][]string{}
		}
		if !keep["creative"] {
			m.Creative = map[string]map[string]string{}
		}
	}
	problems, warnings := validateMedia(cfg, m)
	if err := a.report(problems, warnings, "the media files"); err != nil {
		return 1, err
	}
	shots, previews, creative := m.counts()
	if action == "validate" {
		fmt.Fprintf(a.out, "Media is valid: %d screenshot(s), %d preview(s), %d creative asset slot(s).\n", shots, previews, creative)
		return 0, nil
	}
	if shots+previews+creative == 0 {
		fmt.Fprintf(a.out, "No media in %s/screenshots, previews or creative_assets.\n", cfg.MetadataDir)
		return 0, nil
	}
	c, err := a.client(o)
	if err != nil {
		return 1, err
	}
	r, err := fetchRemote(c, cfg, o.appVersion, false)
	if err != nil {
		return 1, err
	}
	rm, err := fetchMedia(c, r, m)
	if err != nil {
		return 1, err
	}
	p, err := buildMediaPlan(m, r, rm)
	if err != nil {
		return 1, err
	}
	a.show(p, r)
	if action == "diff" {
		if o.exitCode && (len(p.Operations) > 0 || len(p.Locked) > 0) {
			return 2, nil
		}
		return 0, nil
	}
	if len(p.Operations) == 0 {
		fmt.Fprintln(a.out, "\nNothing to push.")
		return 0, nil
	}
	ok, err := a.confirm(fmt.Sprintf("Apply %d media change(s) to App Store Connect? Replaced screenshots and placements are removed.", len(p.Operations)), o.yes)
	if err != nil || !ok {
		return 1, err
	}
	if err := a.execute(c, p); err != nil {
		return 1, err
	}
	fmt.Fprintln(a.out, "Done. Nothing was submitted for review.")
	return 0, nil
}
