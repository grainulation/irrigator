package provider

import (
	"bytes"
	"context"
	"runtime/pprof"
	"strings"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/commandflags"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func goroutineDump(t *testing.T) string {
	t.Helper()
	var buf bytes.Buffer
	if err := pprof.Lookup("goroutine").WriteTo(&buf, 1); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func TestGoLabeledAppliesLabelsVisibleInGoroutineProfile(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	finished := make(chan struct{})
	commandflags.GoLabeled(func() {
		defer close(finished)
		close(started)
		<-release
	}, "irrigator.role", "test-role", "run.id", "run-label-test", "task.id", "t1")
	<-started
	dump := goroutineDump(t)
	for _, want := range []string{`"irrigator.role":"test-role"`, `"run.id":"run-label-test"`, `"task.id":"t1"`} {
		if !strings.Contains(dump, want) {
			t.Fatalf("label %s missing from goroutine profile", want)
		}
	}
	close(release)
	<-finished
}

func TestProviderReaderGoroutineCarriesLabels(t *testing.T) {
	p := fakeProviderPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second) // hang guard
	defer cancel()
	if resp := p.Work(ctx, runfixture.FakeProviderRequest("labelled")); resp.Error != "" {
		t.Fatalf("work: %+v", resp)
	}
	// The slot's process stays alive between requests, so its reader goroutine is parked.
	dump := goroutineDump(t)
	if !strings.Contains(dump, `"irrigator.role":"provider-reader"`) || !strings.Contains(dump, `"provider.model"`) {
		t.Fatalf("provider reader goroutine is unlabeled:\n%s", dump)
	}
}
