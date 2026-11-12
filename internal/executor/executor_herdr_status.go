package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	herdrOK           = "ok"
	herdrTooOld       = "too_old"
	herdrNotInstalled = "not_installed"
	herdrUnknown      = "unknown_version"
)

type herdrInfo struct {
	Status  string
	Path    string
	Version string
	Detail  string
}

var hcVersionPattern = regexp.MustCompile(`^hc\s+v?(\d+)\.(\d+)\.(\d+)`)

// parseHcVersion reads `hc --version` output such as "hc 0.1.19".
func parseHcVersion(out string) (version string, parts [3]int, ok bool) {
	m := hcVersionPattern.FindStringSubmatch(strings.TrimSpace(out))
	if m == nil {
		return "", parts, false
	}
	for i := range parts {
		parts[i], _ = strconv.Atoi(m[i+1])
	}
	return fmt.Sprintf("%d.%d.%d", parts[0], parts[1], parts[2]), parts, true
}

func versionAtLeast(have, min [3]int) bool {
	for i := range have {
		if have[i] != min[i] {
			return have[i] > min[i]
		}
	}
	return true
}

// herdrStatus locates hc and checks its version against herdrMinVersion.
func herdrStatus() herdrInfo {
	path, err := exec.LookPath("hc")
	if err != nil {
		return herdrInfo{Status: herdrNotInstalled}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--version").Output()
	if err != nil {
		return herdrInfo{Status: herdrUnknown, Path: path, Detail: "hc --version failed: " + err.Error()}
	}
	version, have, ok := parseHcVersion(string(out))
	if !ok {
		return herdrInfo{Status: herdrUnknown, Path: path, Detail: fmt.Sprintf("unrecognised version output %q", strings.TrimSpace(string(out)))}
	}
	_, min, _ := parseHcVersion("hc " + HerdrMinVersion)
	if !versionAtLeast(have, min) {
		return herdrInfo{Status: herdrTooOld, Path: path, Version: version}
	}
	return herdrInfo{Status: herdrOK, Path: path, Version: version}
}

// herdrLiveWorkers returns the names of herdr terminals still running a worker
// for this run and cell (any attempt). hc 0.1.19 does not include labels in
// `hc ls` output, so the irrigator.run and irrigator.cell labels are matched
// by hc itself (`--label`); every terminal it returns is a match. A terminal
// that reports no state is treated as live: the question is whether a
// duplicate could start.
func herdrLiveWorkers(hc, runID, cell string) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, hc, herdrLiveWorkerArgs(runID, cell)...).Output()
	if err != nil {
		return nil, fmt.Errorf("hc ls --json: %w", err)
	}
	return liveWorkersFromListing(out, runID, cell)
}

func herdrLiveWorkerArgs(runID, cell string) []string {
	return []string{"ls", "--json", "--label", "irrigator.run=" + runID, "--label", "irrigator.cell=" + cell}
}

// herdrListed is one terminal from `hc ls --json`.
type herdrListed struct {
	Name   string
	State  string
	Labels map[string]string
}

// herdrFinishedStates are the terminal states that mean the worker has ended.
var herdrFinishedStates = []string{"finished", "exited", "dead", "stopped"}

func (h herdrListed) finished() bool { return slices.Contains(herdrFinishedStates, h.State) }

// matchesRun reports whether the entry's own labels, when it has any (newer
// hc), agree with the run and, when cell is non-empty, the cell. hc already
// filtered by label; this only guards against a disagreeing listing.
func (h herdrListed) matchesRun(runID, cell string) bool {
	if len(h.Labels) == 0 {
		return true
	}
	return h.Labels["irrigator.run"] == runID && (cell == "" || h.Labels["irrigator.cell"] == cell)
}

func liveWorkersFromListing(raw []byte, runID, cell string) ([]string, error) {
	listed, err := decodeHerdrListing(raw)
	if err != nil {
		return nil, err
	}
	var live []string
	for _, t := range listed {
		if t.matchesRun(runID, cell) && !t.finished() {
			live = append(live, t.Name)
		}
	}
	return live, nil
}

// decodeHerdrListing parses `hc ls --json`: a top-level array or an object
// holding a terminals/items/data list; labels as an object or key=value list.
func decodeHerdrListing(raw []byte) ([]herdrListed, error) {
	var list []json.RawMessage
	if err := json.Unmarshal(raw, &list); err != nil {
		var wrapped map[string]json.RawMessage
		if json.Unmarshal(raw, &wrapped) != nil {
			return nil, fmt.Errorf("unrecognised hc ls output: %w", err)
		}
		found := false
		for _, key := range []string{"terminals", "items", "data"} {
			if v, ok := wrapped[key]; ok && json.Unmarshal(v, &list) == nil {
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("unrecognised hc ls output: no terminal list")
		}
	}
	var out []herdrListed
	for _, item := range list {
		var t struct {
			Name   string          `json:"name"`
			State  string          `json:"state"`
			Status string          `json:"status"`
			Labels json.RawMessage `json:"labels"`
		}
		if err := json.Unmarshal(item, &t); err != nil {
			return nil, fmt.Errorf("unrecognised hc ls entry: %w", err)
		}
		labels := map[string]string{}
		if json.Unmarshal(t.Labels, &labels) != nil {
			var pairs []string
			if json.Unmarshal(t.Labels, &pairs) == nil {
				for _, kv := range pairs {
					if k, v, ok := strings.Cut(kv, "="); ok {
						labels[k] = v
					}
				}
			}
		}
		out = append(out, herdrListed{Name: t.Name, State: strings.ToLower(t.State + t.Status), Labels: labels})
	}
	return out, nil
}

// doctorExecutors describes each executor for `irrigator doctor`. Not having
// an optional executor's requirements is reported, never an error.
func DoctorExecutors() []map[string]any {
	st := herdrStatus()
	herdr := map[string]any{"name": "herdr", "available": st.Status == herdrOK, "status": st.Status,
		"min_version": HerdrMinVersion, "hc_path": st.Path, "version": st.Version}
	switch st.Status {
	case herdrOK:
		herdr["summary"] = "OK"
	case herdrTooOld:
		herdr["summary"] = fmt.Sprintf("too old: found %s, need >= %s; upgrade from %s", st.Version, HerdrMinVersion, herdrInstallURL)
	case herdrNotInstalled:
		herdr["summary"] = "not installed (optional); see " + herdrInstallURL
	default:
		herdr["summary"] = "version unknown: " + st.Detail
	}
	return []map[string]any{
		{"name": "local", "available": true, "status": herdrOK, "summary": "OK (always available)"},
		herdr,
		tmuxDoctorEntry(),
	}
}
