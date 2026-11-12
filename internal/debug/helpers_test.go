package debug

import (
	"os"
)

// Task replay classes. Everything except the reducer-* classes runs through the
// real worker path.
const (
	rcSkip           = "skip"                // never dispatched: pending behind a blocked dependency
	rcPreCancel      = "pre-cancel"          // cancelled before dispatch (cancelNode)
	rcMockRun        = "mock-run"            // mock executor + real validation: verified or failed
	rcMockInflight   = "mock-cancel-flight"  // cancelled while the mock executor ran
	rcHaikuRun       = "haiku-run"           // stub provider: verified, settled failure with diagnostic code
	rcHaikuTimeout   = "haiku-timeout"       // stub provider hangs: provider timeout -> uncertain
	rcHaikuMalformed = "haiku-malformed"     // stub provider prints garbage -> uncertain
	rcHaikuInflight  = "haiku-cancel-flight" // cancelled while the stub provider ran
	rcHaikuKilled    = "haiku-killed-worker" // worker process SIGKILLed mid-call, supervisor recovery
	rcReducer        = "reducer"             // no executor driver: cell transition reducer
)

func replayTaskClass(fx ReplayFixture, ft ReplayTask) string {
	switch {
	case ft.Attempt == 0 && ft.Status == "cancelled":
		return rcPreCancel
	case ft.Attempt == 0:
		return rcSkip
	case fx.Backend == "managed" && (ft.Status == "verified" || ft.Status == "failed" && ft.FailurePhase == "validation"):
		return rcMockRun // managed mock adapter: the managed worker path, no external call
	case fx.Backend == "mock" && ft.Status == "cancelled":
		return rcMockInflight
	case fx.Backend == "mock" && ft.Status == "verified" || fx.Backend == "mock" && ft.Status == "failed" && ft.FailurePhase == "validation":
		return rcMockRun
	case fx.Backend == "mock" && ft.Status == "waiting":
		return rcMockRun // a waiting parent is a verified spawn producer whose children are not all verified
	case fx.Backend == "haiku" && ft.Status == "verified":
		return rcHaikuRun
	case fx.Backend == "haiku" && ft.Status == "failed" && ft.FailurePhase == "execution" && ft.DiagnosticCode != "":
		return rcHaikuRun
	case fx.Backend == "haiku" && (ft.Status == "cancelled" || ft.CancelRequested):
		return rcHaikuInflight
	case fx.Backend == "haiku" && ft.Status == "uncertain" && ft.ErrorCode == "outcome_uncertain_deadline":
		return rcHaikuTimeout
	case fx.Backend == "haiku" && ft.Status == "uncertain" && ft.ErrorCode == "outcome_uncertain_provider_reported":
		return rcHaikuMalformed
	case fx.Backend == "haiku" && ft.Status == "uncertain" && ft.ErrorCode == replayKilledCode:
		return rcHaikuKilled
	}
	return rcReducer
}

// replayPathOf reports "executor" when every dispatched task replays through the
// real worker path, "reducer" when none does, and "mixed" otherwise.
func replayPathOf(fx ReplayFixture) string {
	exec, red := 0, 0
	for _, ft := range fx.Tasks {
		switch c := replayTaskClass(fx, ft); c {
		case rcSkip:
		case rcReducer:
			red++
		default:
			exec++
		}
	}
	switch {
	case red == 0:
		return "executor"
	case exec == 0:
		return "reducer"
	}
	return "mixed"
}

func loadReplayFixture(path string) (ReplayFixture, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ReplayFixture{}, err
	}
	return DecodeReplayFixture(raw)
}

// replayKilledCode is the error code a SIGKILLed worker's task carries after
// supervisor reconciliation.
const replayKilledCode = "outcome_uncertain_unclassified"
