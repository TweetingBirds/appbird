package main

// The shapes `--json` prints. Field names are part of the CLI's interface: add to them, but
// don't rename or remove one without a major version.

type jsonApp struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	BundleID string `json:"bundle_id"`
	Platform string `json:"platform"`
}

type jsonVersion struct {
	Version  string `json:"version"`
	State    string `json:"state"`
	Editable bool   `json:"editable"`
	Live     bool   `json:"live"`
}

type jsonStatus struct {
	App      jsonApp       `json:"app"`
	Versions []jsonVersion `json:"versions"`
	Locales  []string      `json:"locales"`
	Build    *jsonBuild    `json:"testflight_build"`
}

type jsonChange struct {
	Field  string `json:"field"`
	Old    string `json:"old"`
	New    string `json:"new"`
	Reason string `json:"reason,omitempty"` // why it can't be pushed now
}

type jsonStep struct {
	Step    string       `json:"step"`
	Changes []jsonChange `json:"changes"`
}

type jsonPlan struct {
	App         jsonApp      `json:"app"`
	Version     *jsonVersion `json:"version"`
	Build       *jsonBuild   `json:"testflight_build"`
	Differs     bool         `json:"differs"`
	ChangeCount int          `json:"change_count"`
	Steps       []jsonStep   `json:"steps"`
	Waiting     []jsonChange `json:"waiting"`
	Notes       []string     `json:"notes"`
}

func appJSON(r *Remote) jsonApp {
	return jsonApp{ID: r.App.ID, Name: r.App.Str("name"), BundleID: r.App.Str("bundleId"), Platform: r.Platform}
}

func versionJSON(v *Resource) jsonVersion {
	state := versionState(v)
	return jsonVersion{Version: v.Str("versionString"), State: state,
		Editable: editableVersionStates[state], Live: liveVersionStates[state]}
}

// jsonBuild is the TestFlight build that what_to_test.txt goes to.
type jsonBuild struct {
	Version         string `json:"version"`
	Build           string `json:"build"`
	ProcessingState string `json:"processing_state"`
}

func buildJSON(r *Remote) *jsonBuild {
	if r.Build == nil {
		return nil
	}
	return &jsonBuild{Version: r.BuildVersion, Build: r.Build.Str("version"), ProcessingState: r.Build.Str("processingState")}
}

func statusJSON(r *Remote) jsonStatus {
	out := jsonStatus{App: appJSON(r), Versions: []jsonVersion{}, Locales: sortedKeys(r.VersionLocs), Build: buildJSON(r)}
	for _, v := range r.Versions {
		out.Versions = append(out.Versions, versionJSON(v))
	}
	return out
}

func planJSON(p *Plan, r *Remote) jsonPlan {
	out := jsonPlan{App: appJSON(r), Steps: []jsonStep{}, Waiting: []jsonChange{}, Notes: []string{},
		ChangeCount: p.ChangeCount(), Differs: len(p.Operations) > 0 || len(p.Locked) > 0, Build: buildJSON(r)}
	if r.Version != nil {
		v := versionJSON(r.Version)
		out.Version = &v
	}
	for _, op := range p.Operations {
		step := jsonStep{Step: op.Describe, Changes: []jsonChange{}}
		for _, ch := range op.Changes {
			step.Changes = append(step.Changes, jsonChange{Field: ch.Label, Old: ch.Old, New: ch.New})
		}
		out.Steps = append(out.Steps, step)
	}
	for _, ch := range p.Locked {
		out.Waiting = append(out.Waiting, jsonChange{Field: ch.Label, Old: ch.Old, New: ch.New, Reason: ch.Locked})
	}
	out.Notes = append(out.Notes, p.Notes...)
	return out
}

type jsonCheck struct {
	App             jsonApp      `json:"app"`
	Version         *jsonVersion `json:"version"`
	FirstSubmission bool         `json:"first_submission"`
	Ready           bool         `json:"ready"`
	PendingChanges  int          `json:"pending_changes"` // repo changes to push first
	Problems        []finding    `json:"problems"`
	Warnings        []finding    `json:"warnings"`
	NotChecked      []string     `json:"not_checked"`
}

func checkJSON(rd *readiness, r *Remote) jsonCheck {
	out := jsonCheck{App: appJSON(r), FirstSubmission: rd.First, Ready: rd.Ready(), PendingChanges: rd.Pending,
		Problems: append([]finding{}, rd.Problems...), Warnings: append([]finding{}, rd.Warnings...),
		NotChecked: append([]string{}, rd.NotChecked...)}
	if r.Version != nil {
		v := versionJSON(r.Version)
		out.Version = &v
	}
	return out
}
