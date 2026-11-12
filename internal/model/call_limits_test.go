package model

import (
	"testing"
	"time"
)

func TestSavedCallLimitsResumeDefaultsAndOverrides(t *testing.T) {
	s := &State{}
	defaults := Options{Workers: 2, Timeout: 120 * time.Second, Budget: .25, Retry: false}
	if got := SavedCallOptions(s, defaults, false, false); got != defaults {
		t.Fatal(got)
	}
	SaveCallLimits(s, Options{Workers: 2, Timeout: 600 * time.Second, Budget: .75, Retry: false})
	got := SavedCallOptions(s, defaults, false, false)
	if got.Timeout != 600*time.Second || got.Budget != .75 {
		t.Fatal(got)
	}
	got = SavedCallOptions(s, defaults, true, false)
	if got.Timeout != defaults.Timeout || got.Budget != .75 {
		t.Fatal(got)
	}
	got = SavedCallOptions(s, defaults, false, true)
	if got.Timeout != 600*time.Second || got.Budget != defaults.Budget {
		t.Fatal(got)
	}
	if ValidateCallLimits(&SavedCallLimits{Timeout: -1, Budget: .25}) == nil {
		t.Fatal("invalid limits accepted")
	}
}
