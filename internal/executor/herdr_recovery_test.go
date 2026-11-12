package executor

import (
	"strings"
	"testing"
)

func TestHerdrListingFormats(t *testing.T) {
	for name, raw := range map[string]string{
		"array":        `[{"name":"t1","state":"running","labels":{"irrigator.run":"r","irrigator.cell":"c"}}]`,
		"wrapped":      `{"terminals":[{"name":"t1","state":"running","labels":{"irrigator.run":"r","irrigator.cell":"c"}}]}`,
		"label pairs":  `[{"name":"t1","status":"running","labels":["irrigator.run=r","irrigator.cell=c"]}]`,
		"no state":     `[{"name":"t1","labels":{"irrigator.run":"r","irrigator.cell":"c"}}]`,
		"extra fields": `[{"name":"t1","state":"running","pid":4,"labels":{"irrigator.run":"r","irrigator.cell":"c","irrigator.attempt":"2"}}]`,
		// Real hc 0.1.19: `hc ls --json --label ...` filters by label and omits labels.
		"hc 0.1.19": `{"hidden_finished":0,"terminals":[{"address":"t3g","agent":"irrigator","cwd":"/w","name":"t1","state":"live","turn":0}]}`,
	} {
		live, err := liveWorkersFromListing([]byte(raw), "r", "c")
		if err != nil || len(live) != 1 || live[0] != "t1" {
			t.Errorf("%s: %v %v", name, live, err)
		}
	}
	for name, raw := range map[string]string{
		"finished":   `[{"name":"t1","state":"finished","labels":{"irrigator.run":"r","irrigator.cell":"c"}}]`,
		"other cell": `[{"name":"t1","state":"running","labels":{"irrigator.run":"r","irrigator.cell":"d"}}]`,
		"other run":  `[{"name":"t1","state":"running","labels":{"irrigator.run":"x","irrigator.cell":"c"}}]`,
		"empty":      `[]`,
	} {
		if live, err := liveWorkersFromListing([]byte(raw), "r", "c"); err != nil || len(live) != 0 {
			t.Errorf("%s: %v %v", name, live, err)
		}
	}
	if _, err := liveWorkersFromListing([]byte(`"x"`), "r", "c"); err == nil {
		t.Error("garbage accepted")
	}
}

func TestHerdrLiveWorkerArgsFilterByLabel(t *testing.T) {
	got := strings.Join(herdrLiveWorkerArgs("run-1", "build"), " ")
	if got != "ls --json --label irrigator.run=run-1 --label irrigator.cell=build" {
		t.Fatalf("args = %q", got)
	}
}
