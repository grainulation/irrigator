package model

import (
	"errors"
	"fmt"
	"unicode/utf8"
)

// PlanDisplay is optional producer-authored public metadata for a run.
// Strings are not verified for quality; publication is gated separately.
type PlanDisplay struct {
	Version int    `json:"version"`
	Publish bool   `json:"publish,omitempty"`
	Title   string `json:"title,omitempty"`
	Purpose string `json:"purpose,omitempty"`
}

// TaskDisplay is optional producer-authored public metadata for one task.
// OutcomeField names the single Requires string field that may be published.
type TaskDisplay struct {
	Title        string `json:"title,omitempty"`
	WhySpawned   string `json:"why_spawned,omitempty"`
	DoneWhen     string `json:"done_when,omitempty"`
	OutcomeField string `json:"outcome_field,omitempty"`
}

const (
	displayRunTitleMax  = 60
	displayPurposeMax   = 140
	displayTaskTitleMax = 48
	displayWhyMax       = 100
	displayDoneMax      = 140
	displayOutcomeMax   = 160
)

func displayBound(name, value string, limit int) error {
	if utf8.RuneCountInString(value) > limit {
		return fmt.Errorf("display %s exceeds %d characters", name, limit)
	}
	return nil
}

func ValidatePlanDisplay(p Plan) error {
	if p.Display == nil {
		return nil
	}
	d := p.Display
	if d.Version != 1 {
		return fmt.Errorf("unsupported plan display version %d", d.Version)
	}
	if err := displayBound("title", d.Title, displayRunTitleMax); err != nil {
		return err
	}
	return displayBound("purpose", d.Purpose, displayPurposeMax)
}

func ValidateTaskDisplay(t Task, required, fileFields map[string]bool) error {
	if t.Display == nil {
		return nil
	}
	d := t.Display
	for _, c := range []struct {
		name, value string
		limit       int
	}{{"task title", d.Title, displayTaskTitleMax}, {"why_spawned", d.WhySpawned, displayWhyMax}, {"done_when", d.DoneWhen, displayDoneMax}} {
		if err := displayBound(c.name, c.value, c.limit); err != nil {
			return fmt.Errorf("%s: %w", t.ID, err)
		}
	}
	if d.OutcomeField != "" {
		if !required[d.OutcomeField] {
			return fmt.Errorf("%s: display outcome_field %q must be a required field", t.ID, d.OutcomeField)
		}
		if fileFields[d.OutcomeField] {
			return errors.New(t.ID + ": display outcome_field cannot be a file field")
		}
	}
	return nil
}

// displayPublicationInstructions is added to the direct Haiku prompt only for
// tasks that name an outcome field. It reuses the normal structured output.
func DisplayPublicationInstructions(t Task) map[string]any {
	if t.Display == nil || t.Display.OutcomeField == "" {
		return nil
	}
	return map[string]any{
		"instruction":   "The string in the outcome field below may be shown publicly. Write a plain-text outcome of at most 160 characters. Do not include secrets, private reasoning or file contents.",
		"field_mapping": map[string]string{"public_outcome_field": t.Display.OutcomeField},
	}
}
