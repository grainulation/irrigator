package admission

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
)

// Override lets an operator throttle or hold a run by hand: it reads
// RUN/admission-override.json on each sample. A missing file means no opinion;
// a file that exists but cannot be read or parsed fails closed, so a typo in a
// hold request is visible instead of silently ignored. Fields that are absent
// are no opinion.
type Override struct{ name, path string }

// NewOverride returns the override sensor for the file at path.
func NewOverride(path string) *Override { return &Override{name: "override", path: path} }

// NewOverrideNamed returns a sensor with the same file format under another name,
// for an external hook such as an off-peak or carbon signal.
func NewOverrideNamed(name, path string) *Override { return &Override{name: name, path: path} }

func (o *Override) Name() string { return o.name }

type overrideFile struct {
	Hold           *string `json:"hold"` // none, transient or permanent
	MaxLocalSlots  *int    `json:"max_local_slots"`
	MaxRemoteSlots *int    `json:"max_remote_slots"`
	AllowRemote    *bool   `json:"allow_remote"`
	MaxTier        *string `json:"max_tier"` // minimal, reduced or full: the largest model tier that may start
	Reason         string  `json:"reason"`
}

func (o *Override) Sample(_ context.Context, _ RunSnapshot) Reading {
	r := OpenReading(o.Name())
	raw, err := os.ReadFile(o.path)
	if os.IsNotExist(err) {
		return r
	}
	if err != nil {
		r.Probe, r.ProbeErr = ProbeFailed, err.Error()
		return r
	}
	var f overrideFile
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&f); err != nil {
		r.Probe, r.ProbeErr = ProbeFailed, "invalid override file: "+err.Error()
		return r
	}
	r.Severity, r.Reason = Hard, f.Reason
	if r.Reason == "" {
		r.Reason = "operator override"
	}
	if f.Hold != nil {
		switch *f.Hold {
		case "none":
		case "transient":
			r.Hold = HoldTransient
		case "permanent":
			r.Hold = HoldPermanent
		default:
			r.Probe, r.ProbeErr = ProbeFailed, fmt.Sprintf("override hold %q must be none, transient or permanent", *f.Hold)
			return r
		}
	}
	if f.MaxLocalSlots != nil && *f.MaxLocalSlots >= 0 {
		r.MaxLocalSlots = *f.MaxLocalSlots
	}
	if f.MaxRemoteSlots != nil && *f.MaxRemoteSlots >= 0 {
		r.MaxRemoteSlots = *f.MaxRemoteSlots
	}
	if f.AllowRemote != nil {
		r.AllowRemote = *f.AllowRemote
	}
	if f.MaxTier != nil {
		t, err := ParseTier(*f.MaxTier)
		if err != nil {
			r.Probe, r.ProbeErr = ProbeFailed, err.Error()
			return r
		}
		r.ModelTier = t
	}
	return r
}
