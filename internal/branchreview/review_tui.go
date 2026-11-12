package branchreview

import (
	"errors"
	"fmt"

	"github.com/grainulation/irrigator/internal/reviewtui"
)

// reviewTUIBackend adapts the branch review functions behind `review list`,
// `review diff`, `review approve` and `review request-changes` to the terminal
// UI. It adds no storage of its own: verdicts are written by setChangeVerdict.
type reviewTUIBackend struct{ ws, base string }

func reviewTUIStatus(v *changeVerdictView) string {
	switch {
	case v == nil:
		return "pending"
	case !v.Current:
		return "pending (head moved)"
	case v.State == "approved":
		return "approved"
	default:
		return "changes requested"
	}
}

func (b reviewTUIBackend) Queue() (string, []reviewtui.Item, error) {
	q, err := ListReviews(b.ws, b.base)
	if err != nil {
		return "", nil, err
	}
	overlap := map[string]bool{}
	for _, o := range q.Overlaps {
		for _, branch := range o[1:] {
			overlap[branch] = true
		}
	}
	items := make([]reviewtui.Item, 0, len(q.Changes))
	for _, c := range q.Changes {
		items = append(items, reviewtui.Item{Branch: c.Branch, Head: c.Head, Ahead: c.Ahead, Files: c.Files, Latest: c.Latest,
			Status: reviewTUIStatus(c.Verdict), Overlap: overlap[c.Branch], Current: c.Current})
	}
	return q.Base, items, nil
}

// pinned refuses when the branch moved since the reviewer's queue was loaded:
// a verdict binds to the head at write time, which must be the head they saw.
func (b reviewTUIBackend) pinned(branch, head string) error {
	name, err := changeBranch(b.ws, branch)
	if err != nil {
		return err
	}
	now, err := changeRev(b.ws, "refs/heads/"+name)
	if err != nil {
		return err
	}
	if now != head {
		return fmt.Errorf("%s moved since it was listed; refresh with R and review the new head", branch)
	}
	return nil
}

// Diff returns the raw, unsanitized Git diff for the pinned head. The review UI
// is the single presentation boundary: it sanitizes each display line itself,
// so sanitizing here as well would double literal backslashes and hide CRLF.
func (b reviewTUIBackend) Diff(branch, head string) (string, error) {
	if err := b.pinned(branch, head); err != nil {
		return "", err
	}
	return rawChangeDiff(b.ws, branch, b.base, "", "", head)
}

func (b reviewTUIBackend) Approve(branch, head string) error {
	if err := b.pinned(branch, head); err != nil {
		return err
	}
	_, _, err := setChangeVerdict(b.ws, branch, b.base, "approved", "")
	return err
}

func (b reviewTUIBackend) RequestChanges(branch, head, note string) error {
	if err := b.pinned(branch, head); err != nil {
		return err
	}
	_, _, err := setChangeVerdict(b.ws, branch, b.base, "changes_requested", note)
	return err
}

func reviewTUI(ws, base string) (int, error) {
	if err := reviewtui.Run(reviewTUIBackend{ws: ws, base: base}); err != nil {
		if errors.Is(err, reviewtui.ErrNotTerminal) {
			return 2, err
		}
		return 1, err
	}
	return 0, nil
}
