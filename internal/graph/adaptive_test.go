package graph

import (
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/model"
)

var polBase = time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)

func polState(revision int) *model.State {
	return &model.State{Version: 1, GraphRevision: revision, Tasks: map[string]*model.TaskState{}}
}

// polProposal is admissible under the default policy: net benefit 300-0-0 = 300ms.
func polProposal(base int) model.RevisionProposal {
	return model.RevisionProposal{ID: "p1", BaseRevision: base, ExpectedSavingsMS: 300, Confidence: 0.9}
}

func TestAdaptivePolicyValidationBoundaries(t *testing.T) {
	if got := DefaultAdaptivePolicy(); got != (model.AdaptivePolicy{MinBenefitMS: 50, MinConfidence: 0.6, CostPenaltyMSPerUSD: 1000, MaxExtraCostUSD: 0.25, MaxTotalCostUSD: 5, CooldownMS: 1000, MaxRevisions: 8}) {
		t.Fatalf("default policy drifted: %+v", got)
	}
	if err := checkpoint.ValidateAdaptivePolicy(DefaultAdaptivePolicy()); err != nil {
		t.Fatalf("default policy rejected: %v", err)
	}
	nan := math.NaN()
	inf := math.Inf(1)
	cases := []struct {
		name    string
		mutate  func(*model.AdaptivePolicy)
		wantErr bool
	}{
		{"min benefit zero", func(p *model.AdaptivePolicy) { p.MinBenefitMS = 0 }, false},
		{"min benefit max", func(p *model.AdaptivePolicy) { p.MinBenefitMS = 3600000 }, false},
		{"min benefit above max", func(p *model.AdaptivePolicy) { p.MinBenefitMS = 3600000.5 }, true},
		{"min benefit negative", func(p *model.AdaptivePolicy) { p.MinBenefitMS = -0.001 }, true},
		{"min benefit NaN", func(p *model.AdaptivePolicy) { p.MinBenefitMS = nan }, true},
		{"min benefit Inf", func(p *model.AdaptivePolicy) { p.MinBenefitMS = inf }, true},
		{"confidence zero", func(p *model.AdaptivePolicy) { p.MinConfidence = 0 }, false},
		{"confidence one", func(p *model.AdaptivePolicy) { p.MinConfidence = 1 }, false},
		{"confidence above one", func(p *model.AdaptivePolicy) { p.MinConfidence = math.Nextafter(1, 2) }, true},
		{"confidence negative", func(p *model.AdaptivePolicy) { p.MinConfidence = -0.1 }, true},
		{"confidence NaN", func(p *model.AdaptivePolicy) { p.MinConfidence = nan }, true},
		{"penalty zero", func(p *model.AdaptivePolicy) { p.CostPenaltyMSPerUSD = 0 }, false},
		{"penalty max", func(p *model.AdaptivePolicy) { p.CostPenaltyMSPerUSD = 1000000 }, false},
		{"penalty above max", func(p *model.AdaptivePolicy) { p.CostPenaltyMSPerUSD = 1000001 }, true},
		{"penalty negative", func(p *model.AdaptivePolicy) { p.CostPenaltyMSPerUSD = -1 }, true},
		{"penalty Inf", func(p *model.AdaptivePolicy) { p.CostPenaltyMSPerUSD = inf }, true},
		{"extra cost max", func(p *model.AdaptivePolicy) { p.MaxExtraCostUSD = 1000 }, false},
		{"extra cost above max", func(p *model.AdaptivePolicy) { p.MaxExtraCostUSD = 1000.001 }, true},
		{"extra cost negative", func(p *model.AdaptivePolicy) { p.MaxExtraCostUSD = -0.5 }, true},
		{"extra cost NaN", func(p *model.AdaptivePolicy) { p.MaxExtraCostUSD = nan }, true},
		{"total cost zero", func(p *model.AdaptivePolicy) { p.MaxTotalCostUSD = 0 }, false},
		{"total cost above max", func(p *model.AdaptivePolicy) { p.MaxTotalCostUSD = 1000.001 }, true},
		{"total cost NaN", func(p *model.AdaptivePolicy) { p.MaxTotalCostUSD = nan }, true},
		{"cooldown zero", func(p *model.AdaptivePolicy) { p.CooldownMS = 0 }, false},
		{"cooldown one day", func(p *model.AdaptivePolicy) { p.CooldownMS = 86400000 }, false},
		{"cooldown above one day", func(p *model.AdaptivePolicy) { p.CooldownMS = 86400001 }, true},
		{"cooldown negative", func(p *model.AdaptivePolicy) { p.CooldownMS = -1 }, true},
		{"revisions zero", func(p *model.AdaptivePolicy) { p.MaxRevisions = 0 }, false},
		{"revisions max", func(p *model.AdaptivePolicy) { p.MaxRevisions = 64 }, false},
		{"revisions above max", func(p *model.AdaptivePolicy) { p.MaxRevisions = 65 }, true},
		{"revisions negative", func(p *model.AdaptivePolicy) { p.MaxRevisions = -1 }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := DefaultAdaptivePolicy()
			tc.mutate(&p)
			err := checkpoint.ValidateAdaptivePolicy(p)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateAdaptivePolicy error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestAdaptiveDecisionBoundaries(t *testing.T) {
	cases := []struct {
		name       string
		setup      func(*model.State, *model.RevisionProposal)
		wantAction string
		wantReason string
	}{
		{"baseline applies", func(*model.State, *model.RevisionProposal) {}, "apply", "accepted"},
		{"net benefit exactly at minimum applies", func(_ *model.State, p *model.RevisionProposal) {
			p.ExpectedSavingsMS, p.ExtraCostUSD = 300, 0.25 // 300 - 0.25*1000 = 50
		}, "apply", "accepted"},
		{"net benefit just below minimum rejects", func(_ *model.State, p *model.RevisionProposal) {
			p.ExpectedSavingsMS, p.ExtraCostUSD = 299.999, 0.25
		}, "reject", "net benefit below minimum"},
		{"switch cost reduces benefit", func(_ *model.State, p *model.RevisionProposal) {
			p.SwitchCostMS = 251
		}, "reject", "net benefit below minimum"},
		{"confidence exactly at minimum applies", func(_ *model.State, p *model.RevisionProposal) {
			p.Confidence = 0.6
		}, "apply", "accepted"},
		{"confidence just below minimum rejects", func(_ *model.State, p *model.RevisionProposal) {
			p.Confidence = math.Nextafter(0.6, 0)
		}, "reject", "confidence below minimum"},
		{"extra cost exactly at cap applies", func(_ *model.State, p *model.RevisionProposal) {
			p.ExpectedSavingsMS, p.ExtraCostUSD = 300, 0.25
		}, "apply", "accepted"},
		{"extra cost above cap rejects", func(_ *model.State, p *model.RevisionProposal) {
			p.ExpectedSavingsMS, p.ExtraCostUSD = 1e6, math.Nextafter(0.25, 1)
		}, "reject", "extra cost exceeds per-proposal maximum"},
		{"total cost exactly at cap applies", func(s *model.State, p *model.RevisionProposal) {
			s.Tasks["a"] = &model.TaskState{Cost: 4.75}
			p.ExpectedSavingsMS, p.ExtraCostUSD = 300, 0.25 // 4.75 + 0.25 = 5
		}, "apply", "accepted"},
		{"total cost above cap rejects", func(s *model.State, p *model.RevisionProposal) {
			s.Tasks["a"] = &model.TaskState{Cost: 4.8}
			p.ExpectedSavingsMS, p.ExtraCostUSD = 300, 0.25 // 4.8 + 0.25 = 5.05
		}, "reject", "total reported cost"},
		{"archived cost counts toward total cap", func(s *model.State, p *model.RevisionProposal) {
			s.ArchivedTasks = []model.TaskState{{Cost: 4.8}}
			p.ExpectedSavingsMS, p.ExtraCostUSD = 300, 0.25
		}, "reject", "total reported cost"},
		{"revision limit below max applies", func(s *model.State, p *model.RevisionProposal) {
			s.GraphRevision, p.BaseRevision = 7, 7
		}, "apply", "accepted"},
		{"revision limit reached rejects", func(s *model.State, p *model.RevisionProposal) {
			s.GraphRevision, p.BaseRevision = 8, 8
		}, "reject", "revision limit 8 reached"},
		{"stale base revision rejects", func(s *model.State, p *model.RevisionProposal) {
			s.GraphRevision, p.BaseRevision = 2, 1
		}, "reject", "stale base revision 1; current revision is 2"},
		{"cooldown one ms short defers", func(s *model.State, p *model.RevisionProposal) {
			s.Revisions = []model.GraphRevisionRecord{{Revision: 1, At: polBase.Add(-999 * time.Millisecond)}}
			s.GraphRevision, p.BaseRevision = 1, 1
		}, "defer", "cooldown active since last revision"},
		{"cooldown exactly elapsed applies", func(s *model.State, p *model.RevisionProposal) {
			s.Revisions = []model.GraphRevisionRecord{{Revision: 1, At: polBase.Add(-1000 * time.Millisecond)}}
			s.GraphRevision, p.BaseRevision = 1, 1
		}, "apply", "accepted"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := polState(0)
			p := polProposal(0)
			tc.setup(s, &p)
			d := adaptiveDecision(s, p, polBase)
			if d.Action != tc.wantAction || !strings.Contains(d.Reason, tc.wantReason) {
				t.Fatalf("decision = %+v; want action %q reason containing %q", d, tc.wantAction, tc.wantReason)
			}
			if d.ProposalID != p.ID || d.BaseRevision != p.BaseRevision || !d.Time.Equal(polBase) {
				t.Fatalf("decision identity fields not carried through: %+v", d)
			}
		})
	}
}

func TestAdaptiveDecisionNetBenefitAndPolicyOverride(t *testing.T) {
	s := polState(0)
	p := polProposal(0)
	p.ExpectedSavingsMS, p.SwitchCostMS, p.ExtraCostUSD = 500, 100, 0.2
	d := adaptiveDecision(s, p, polBase)
	if want := p.ExpectedSavingsMS - p.SwitchCostMS - p.ExtraCostUSD*1000; d.NetBenefitMS != want {
		t.Fatalf("NetBenefitMS = %v, want %v", d.NetBenefitMS, want)
	}

	s.Policy = &model.AdaptivePolicy{MinBenefitMS: 0, MinConfidence: 0.95, CostPenaltyMSPerUSD: 0, MaxExtraCostUSD: 1, MaxTotalCostUSD: 1000, CooldownMS: 0, MaxRevisions: 1}
	if d := adaptiveDecision(s, polProposal(0), polBase); d.Action != "reject" || !strings.Contains(d.Reason, "confidence below minimum") {
		t.Fatalf("explicit policy confidence not enforced: %+v", d)
	}
	s.Policy.MinConfidence = 0.9
	if d := adaptiveDecision(s, polProposal(0), polBase); d.Action != "apply" {
		t.Fatalf("explicit policy should admit proposal: %+v", d)
	}
}

func TestAdaptiveDecisionRejectsInvalidInputs(t *testing.T) {
	nan := math.NaN()
	inf := math.Inf(1)
	cases := []struct {
		name   string
		mutate func(*model.RevisionProposal)
	}{
		{"NaN expected savings", func(p *model.RevisionProposal) { p.ExpectedSavingsMS = nan }},
		{"Inf expected savings", func(p *model.RevisionProposal) { p.ExpectedSavingsMS = inf }},
		{"NaN switch cost", func(p *model.RevisionProposal) { p.SwitchCostMS = nan }},
		{"negative switch cost", func(p *model.RevisionProposal) { p.SwitchCostMS = -1 }},
		{"Inf extra cost", func(p *model.RevisionProposal) { p.ExtraCostUSD = inf }},
		{"negative extra cost", func(p *model.RevisionProposal) { p.ExtraCostUSD = -0.01 }},
		{"NaN confidence", func(p *model.RevisionProposal) { p.Confidence = nan }},
		{"confidence above one", func(p *model.RevisionProposal) { p.Confidence = 1.0000001 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := polProposal(0)
			tc.mutate(&p)
			d := adaptiveDecision(polState(0), p, polBase)
			if d.Action != "reject" || !strings.HasPrefix(d.Reason, "invalid proposal") {
				t.Fatalf("decision = %+v; want invalid proposal rejection", d)
			}
		})
	}

	s := polState(0)
	s.Policy = &model.AdaptivePolicy{MinConfidence: nan}
	if d := adaptiveDecision(s, polProposal(0), polBase); d.Action != "reject" || !strings.HasPrefix(d.Reason, "invalid policy") {
		t.Fatalf("invalid stored policy not rejected: %+v", d)
	}
}

func TestAdaptiveDecisionReasonPrecedence(t *testing.T) {
	// Stale base and low confidence both apply; the stale reason is reported
	// because the base revision check runs first.
	s := polState(3)
	p := polProposal(2)
	p.Confidence = 0
	if d := adaptiveDecision(s, p, polBase); !strings.Contains(d.Reason, "stale base revision") {
		t.Fatalf("precedence changed: %+v", d)
	}
}

func TestAdaptiveDecisionIsDeterministicAndPure(t *testing.T) {
	build := func() *model.State {
		s := polState(2)
		s.Tasks["a"] = &model.TaskState{Cost: 0.1, Status: "verified"}
		s.Tasks["b"] = &model.TaskState{Cost: 0.2, Status: "running"}
		s.Tasks["c"] = &model.TaskState{Cost: 0.3, Status: "verified"}
		s.ArchivedTasks = []model.TaskState{{Cost: 0.4}}
		s.Revisions = []model.GraphRevisionRecord{{Revision: 1, At: polBase.Add(-time.Hour)}, {Revision: 2, At: polBase.Add(-time.Hour)}}
		return s
	}
	p := polProposal(2)
	p.ExtraCostUSD = 0.1
	s := build()
	first := adaptiveDecision(s, p, polBase)
	for i := 0; i < 50; i++ {
		if got := adaptiveDecision(s, p, polBase); got != first {
			t.Fatalf("run %d = %+v, want %+v", i, got, first)
		}
	}
	if !reflect.DeepEqual(s, build()) {
		t.Fatal("adaptiveDecision mutated state")
	}
}

func TestAdaptiveTotalCostAccounting(t *testing.T) {
	if got := model.AdaptiveTotalCost(polState(0)); got != 0 {
		t.Fatalf("empty total = %v, want 0", got)
	}
	s := polState(0)
	s.Tasks["a"] = &model.TaskState{Cost: 1.5}
	s.Tasks["b"] = &model.TaskState{Cost: 0.25}
	s.Tasks["nil"] = nil
	// Archived generation of "a" is a separate record and counts once.
	s.ArchivedTasks = []model.TaskState{{Spec: model.Task{ID: "a"}, Cost: 2}, {Spec: model.Task{ID: "a"}, Cost: 0.75}}
	if got := model.AdaptiveTotalCost(s); got != 4.5 {
		t.Fatalf("total = %v, want 4.5", got)
	}
	// Float addition is order-sensitive; repeated calls must be bit-identical.
	s = polState(0)
	for _, id := range []string{"x", "y", "z", "w", "v"} {
		s.Tasks[id] = &model.TaskState{Cost: 0.1}
	}
	s.ArchivedTasks = []model.TaskState{{Cost: 0.2}, {Cost: 0.3}}
	want := model.AdaptiveTotalCost(s)
	for i := 0; i < 100; i++ {
		if got := model.AdaptiveTotalCost(s); got != want {
			t.Fatalf("run %d total = %v, want %v", i, got, want)
		}
	}
}

func TestRecordAdaptiveOutcome(t *testing.T) {
	t.Run("no revisions is a no-op", func(t *testing.T) {
		s := polState(0)
		RecordAdaptiveOutcome(s, "complete", polBase)
		if len(s.Revisions) != 0 {
			t.Fatalf("revisions created: %+v", s.Revisions)
		}
	})

	build := func() *model.State {
		s := polState(2)
		s.Tasks["a"] = &model.TaskState{Cost: 1.5, Status: "verified"}
		s.Tasks["b"] = &model.TaskState{Cost: 0.75, Status: "failed"}
		s.Tasks["c"] = &model.TaskState{Status: "running"}
		s.ArchivedTasks = []model.TaskState{{Cost: 0.25}}
		s.Revisions = []model.GraphRevisionRecord{
			{Revision: 1, At: polBase.Add(-time.Minute), CostAtCommitUSD: 1, PredictedGainMS: 500},
			{Revision: 2, At: polBase.Add(10 * time.Second), CostAtCommitUSD: 2, PredictedGainMS: 100},
		}
		return s
	}

	t.Run("attaches to last revision only", func(t *testing.T) {
		s := build()
		now := polBase.Add(10*time.Second + 1500*time.Millisecond)
		RecordAdaptiveOutcome(s, "complete", now)
		want := &model.AdaptiveOutcome{Status: "complete", ObservedAt: now, ElapsedMS: 1500, ReportedCostUSD: 0.5, VerifiedNodes: 1, FailedNodes: 1}
		if !reflect.DeepEqual(s.Revisions[1].Outcome, want) {
			t.Fatalf("outcome = %+v, want %+v", s.Revisions[1].Outcome, want)
		}
		if s.Revisions[0].Outcome != nil {
			t.Fatalf("earlier revision outcome changed: %+v", s.Revisions[0].Outcome)
		}
		// Observation is recorded without asserting causal improvement.
		if s.Revisions[1].PredictedGainMS != 100 || s.GraphRevision != 2 || len(s.Revisions) != 2 {
			t.Fatalf("revision chain changed: %+v", s.Revisions)
		}
	})

	t.Run("blocked status and repeated calls are idempotent", func(t *testing.T) {
		s := build()
		RecordAdaptiveOutcome(s, "blocked", polBase.Add(time.Second))
		first := *s.Revisions[1].Outcome
		RecordAdaptiveOutcome(s, "complete", polBase.Add(time.Hour))
		if got := *s.Revisions[1].Outcome; !reflect.DeepEqual(got, first) || got.Status != "blocked" {
			t.Fatalf("second call overwrote outcome: %+v, first %+v", got, first)
		}
	})

	t.Run("negative elapsed and cost deltas clamp to zero", func(t *testing.T) {
		s := build()
		s.Revisions[1].CostAtCommitUSD = 10
		RecordAdaptiveOutcome(s, "complete", polBase)
		got := s.Revisions[1].Outcome
		if got.ElapsedMS != 0 || got.ReportedCostUSD != 0 {
			t.Fatalf("outcome = %+v, want zero elapsed and zero cost", got)
		}
	})
}
