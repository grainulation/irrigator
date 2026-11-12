package graph

import (
	"testing"

	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func TestTurnRevisionRejectsHistoryAndTopology(t *testing.T) {
	for _, kind := range []string{"running", "verified", "uncertain", "attempted", "identity", "dependency", "add", "weaken"} {
		t.Run(kind, func(t *testing.T) {
			root := runfixture.TurnFixture(t, model.TurnShape{Roots: 1, MainTurns: 2, HelpersPerTurn: 1}, runfixture.Task("a", 0))
			s := runfixture.Read(t, root)
			tasks := s.AllTasks()
			tasks[0].Prompt = "change"
			switch kind {
			case "running", "verified", "uncertain":
				s.Tasks["a"].Status = kind
			case "attempted":
				s.Tasks["a"].Attempt = 1
			case "identity":
				x := *tasks[0].Turn
				x.AgentID = "other"
				tasks[0].Turn = &x
			case "dependency":
				tasks[0].DependsOn = []string{"other"}
			case "add":
				tasks = append(tasks, runfixture.Task("extra", 0))
			case "weaken":
				tasks[0].Requires = nil
			}
			if ValidateTurnRevision(s, tasks) == nil {
				t.Fatal("unsafe revision accepted")
			}
		})
	}
}
