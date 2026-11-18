package provider

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/localfs"
	"github.com/grainulation/irrigator/internal/model"
)

type limitAdvisory struct {
	Schema         string     `json:"schema"`
	RunID          string     `json:"run_id"`
	Task           string     `json:"task"`
	Attempt        int        `json:"attempt"`
	BindingDigest  string     `json:"binding_digest,omitempty"`
	Status         string     `json:"status"`
	ResetAt        *time.Time `json:"reset_at"`
	ResetBasis     string     `json:"reset_basis"`
	ObservedAt     *time.Time `json:"observed_at"`
	Sources        []string   `json:"sources"`
	ReadErrors     []string   `json:"read_errors"`
	DiagnosticCode string     `json:"diagnostic_code,omitempty"`
	AdvisoryOnly   bool       `json:"advisory_only"`
	AutomaticRetry bool       `json:"automatic_retry"`
}

var limitSignal = regexp.MustCompile(`(?i)(rate[_ -]?limit(?:ed|[_ -]?(?:error|exceed\w*|reach\w*))|too many requests|usage limit.{0,30}(?:reach|exceed|exhaust)|quota.{0,30}(exceed|exhaust)|(?:http|status[_ -]?code)[" :]*(429)\b)`)
var resetISO = regexp.MustCompile(`(?i)(?:reset(?:s)?(?:_at|At)?|retry[_ -]?after|until)["\s:=]+(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2}))`)
var resetEpoch = regexp.MustCompile(`(?i)["']?(?:resets?_at|resetsAt)["']?\s*:\s*(\d{10,13})\b`)
var resetRelative = regexp.MustCompile(`(?i)(?:retry[_ -]?after|resets? in|try again in)["\s:=]+([^\s;}"\]]+)(?:[ \t]+([a-z]+))?`)
var resetDuration = regexp.MustCompile(`(?i)^([0-9]{1,7})(seconds?|secs?|s|minutes?|mins?|m|hours?|hrs?|h)?$`)

// limitResetCandidates never emits raw provider output and never grants replay.
func limitResetCandidates(raw string, observed time.Time) []time.Time {
	times := map[int64]time.Time{}
	add := func(at time.Time) {
		if !at.Before(observed.Add(-time.Minute)) && !at.After(observed.Add(31*24*time.Hour)) {
			times[at.Unix()] = at.UTC().Truncate(time.Second)
		}
	}
	for _, m := range resetISO.FindAllStringSubmatch(raw, -1) {
		if at, err := time.Parse(time.RFC3339, m[1]); err == nil {
			add(at)
		}
	}
	for _, m := range resetEpoch.FindAllStringSubmatch(raw, -1) {
		n, _ := strconv.ParseInt(m[1], 10, 64)
		if n > 1e12 {
			add(time.UnixMilli(n))
		} else {
			add(time.Unix(n, 0))
		}
	}
	for _, m := range resetRelative.FindAllStringSubmatch(raw, -1) {
		// Parse the complete token. Prefix matches turn dates/fractions into false
		// durations (2026 seconds or 1 second); unsupported values stay unknown.
		duration := resetDuration.FindStringSubmatch(strings.TrimSuffix(m[1], ","))
		if duration == nil {
			continue
		}
		unitName := strings.ToLower(duration[2])
		if m[2] != "" {
			if unitName != "" || resetDuration.FindStringSubmatch("1"+m[2]) == nil {
				continue
			}
			unitName = strings.ToLower(m[2])
		}
		n, _ := strconv.ParseInt(duration[1], 10, 64)
		unit := time.Second
		if strings.HasPrefix(unitName, "m") {
			unit = time.Minute
		}
		if strings.HasPrefix(unitName, "h") {
			unit = time.Hour
		}
		if n > 0 && n <= int64(31*24*time.Hour/unit) {
			add(observed.Add(time.Duration(n) * unit))
		}
	}
	result := []time.Time{}
	for _, at := range times {
		result = append(result, at)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Before(result[j]) })
	return result
}

func readLimitSource(path string, limit int64) ([]byte, time.Time, error) {
	f, err := localfs.OpenContextFile(path, os.O_RDONLY, 0)
	if err != nil {
		return nil, time.Time{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, time.Time{}, err
	}
	if !info.Mode().IsRegular() {
		return nil, time.Time{}, errors.New("source is not regular")
	}
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, time.Time{}, err
	}
	if int64(len(raw)) > limit {
		return nil, time.Time{}, errors.New("source exceeds bound")
	}
	return raw, info.ModTime().UTC(), nil
}

// DiagnoseLimit reads only one known attempt's captured provider output. A reset
// is a provider-output advisory, not account verification or permission to retry.
func DiagnoseLimit(root, task string, attempt int, now time.Time) (limitAdvisory, error) {
	var result limitAdvisory
	root, err := filepath.Abs(root)
	if err != nil {
		return result, err
	}
	raw, _, err := readLimitSource(filepath.Join(root, "state.json"), 16<<20)
	if err != nil {
		return result, err
	}
	s, err := checkpoint.DecodeState(root, raw)
	if err != nil {
		return result, err
	}
	row := s.Tasks[task]
	if row == nil || attempt < 1 || attempt > row.Attempt {
		return result, errors.New("unknown task or attempt")
	}
	result = limitAdvisory{Schema: "irrigator.provider-limit/v1", RunID: s.RunID, Task: task, Attempt: attempt, Status: "no_limit_evidence", ResetBasis: "unknown", Sources: []string{}, ReadErrors: []string{}, AdvisoryOnly: true}
	if attempt == row.Attempt {
		result.BindingDigest = row.BindingDigest
		result.DiagnosticCode = row.DiagnosticCode
	}
	resetCandidates := map[int64]time.Time{}
	for _, suffix := range []string{".stdout.json", ".stderr.log"} {
		name := fmt.Sprintf("%d%s", attempt, suffix)
		raw, observed, err := readLimitSource(filepath.Join(root, "cells", task, name), 4<<20)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			result.ReadErrors = append(result.ReadErrors, name+": unavailable")
			continue
		}
		result.Sources = append(result.Sources, name)
		if !limitSignal.Match(raw) {
			continue
		}
		result.Status = "limited_reset_unknown"
		if result.ObservedAt == nil || observed.After(*result.ObservedAt) {
			value := observed
			result.ObservedAt = &value
		}
		for _, at := range limitResetCandidates(string(raw), observed) {
			resetCandidates[at.Unix()] = at
		}
	}
	if len(resetCandidates) == 1 {
		for _, at := range resetCandidates {
			value := at
			result.ResetAt = &value
		}
		result.ResetBasis = "captured_output_estimate"
		if result.ResetAt.After(now) {
			result.Status = "cooldown_advisory"
		} else {
			result.Status = "reset_elapsed_advisory"
		}
	}
	if len(resetCandidates) > 1 {
		result.ResetBasis = "conflicting_estimates"
	}
	if len(result.Sources) == 0 {
		result.Status = "unavailable"
	}
	// A current immutable budget diagnostic is separate from account quota.
	if attempt == row.Attempt && (row.DiagnosticCode == model.DiagProviderSessionBudget || row.DiagnosticCode == model.DiagProviderOperationBudget) {
		result.Status = "operation_or_session_budget"
		result.ResetAt = nil
		result.ResetBasis = "not_account_quota"
	}
	return result, nil
}
