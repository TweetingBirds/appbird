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

func statusJSON(r *Remote) jsonStatus {
	out := jsonStatus{App: appJSON(r), Versions: []jsonVersion{}, Locales: sortedKeys(r.VersionLocs)}
	for _, v := range r.Versions {
		out.Versions = append(out.Versions, versionJSON(v))
	}
	return out
}

func planJSON(p *Plan, r *Remote) jsonPlan {
	out := jsonPlan{App: appJSON(r), Steps: []jsonStep{}, Waiting: []jsonChange{}, Notes: []string{},
		ChangeCount: p.ChangeCount(), Differs: len(p.Operations) > 0 || len(p.Locked) > 0}
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
