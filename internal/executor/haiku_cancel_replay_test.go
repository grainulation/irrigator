package executor

import (
	"context"
	"testing"

	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func TestKnownNotStartedHaikuDoesNotInventCharge(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, cost, err := callHaiku(context.Background(), t.TempDir(), 1, runfixture.Task("a", 0), nil, .25)
	if !model.KnownNotStarted(err) || cost != 0 {
		t.Fatalf("pre-dispatch failure not preserved: %v %v", cost, err)
	}
}
