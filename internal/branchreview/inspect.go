package branchreview

import (
	"errors"
	"fmt"
	"strings"

	"github.com/grainulation/irrigator/internal/session"
)

// InspectReview reads a queued branch or document without recording viewed
// marks, comments or decisions. Snapshot is the ID listed by ListReviews, not
// an arbitrary directory. Branch diffs are pinned to the returned head.
func InspectReview(ws, branch, base, snapshot, file string) (map[string]any, error) {
	if (branch == "") == (snapshot == "") {
		return nil, errors.New("provide exactly one of branch or snapshot")
	}
	if branch != "" {
		v, _, err := loadChange(ws, branch, base)
		if err != nil {
			return nil, err
		}
		return inspectBranchReview(ws, v, file)
	}
	if base != "" || file != "" {
		return nil, errors.New("base and file apply only to branch inspection")
	}
	if len(snapshot) != 64 || strings.ContainsAny(snapshot, "/\\\x00") {
		return nil, errors.New("snapshot must be a queued snapshot ID")
	}
	for _, ch := range snapshot {
		if !strings.ContainsRune("0123456789abcdef", ch) {
			return nil, errors.New("snapshot must be a queued snapshot ID")
		}
	}
	documents, err := listReviewDocuments(ws)
	if err != nil {
		return nil, err
	}
	dir := ""
	for _, document := range documents {
		if document.Snapshot == snapshot {
			dir = document.Path
			break
		}
	}
	if dir == "" {
		return nil, errors.New("snapshot is not in the workspace review queue")
	}
	s, err := readReview(dir)
	if err != nil {
		return nil, err
	}
	if s.ID != snapshot || reviewKind(s) == "code" {
		return nil, errors.New("snapshot is not a queued review document")
	}
	comments, err := reviewComments(dir)
	if err != nil {
		return nil, err
	}
	if _, err := reviewFeedback(s, comments); err != nil {
		return nil, err
	}
	// Avoid returning retained binary inventory; queued documents need their
	// body, identity and journal, rather than an entire capture workspace.
	return map[string]any{"kind": "document", "review": map[string]any{"id": s.ID, "kind": s.Kind, "title": s.Title, "body": s.Body, "captured_at": s.CapturedAt, "comments": comments}}, nil
}

// inspectBranchReview uses the same immutable comparison as the returned
// metadata, even if either branch ref moves while the Git diff is being read.
func inspectBranchReview(ws string, v *changeView, file string) (map[string]any, error) {
	if len(v.Commits) == 0 {
		return nil, fmt.Errorf("%s is not ahead of its base and has no queued review", v.Branch)
	}
	if file != "" && !reviewPath(file) {
		return nil, errors.New("invalid file path")
	}
	args := []string{"diff", "--no-ext-diff", "--no-textconv", "--no-color", "--submodule=short", "--src-prefix=a/", "--dst-prefix=b/", "--output-indicator-new=+", "--output-indicator-old=-", "--output-indicator-context= ", "-M"}
	if file == "" {
		args = append(args, v.MergeBase, v.Head, "--")
		diff, err := session.ReviewGit(ws, args...)
		if err != nil {
			return nil, err
		}
		return map[string]any{"kind": "branch", "review": v, "diff": session.ReviewTerminalText(string(diff))}, nil
	}
	var selected *changeFile
	for i := range v.Files {
		if v.Files[i].Path == file {
			selected = &v.Files[i]
			break
		}
	}
	if selected == nil {
		for i := range v.Files {
			if v.Files[i].OldPath == file {
				selected = &v.Files[i]
				break
			}
		}
	}
	if selected == nil {
		return nil, errors.New("file is not part of this queued change")
	}
	// Raw NUL-delimited records identify patch blocks without interpreting Git's
	// quoted filenames or treating a file name as a recursive directory pathspec.
	args = append(args, "--raw", "-z", "--patch", v.MergeBase, v.Head, "--")
	diff, err := session.ReviewGit(ws, args...)
	if err != nil {
		return nil, err
	}
	patch, err := selectedReviewPatch(string(diff), selected)
	if err != nil {
		return nil, err
	}
	return map[string]any{"kind": "branch", "review": v, "diff": session.ReviewTerminalText(patch)}, nil
}

func selectedReviewPatch(raw string, selected *changeFile) (string, error) {
	metadata, patch, ok := strings.Cut(raw, "\x00\x00")
	if !ok {
		return "", errors.New("missing Git raw/patch boundary")
	}
	fields := strings.Split(metadata, "\x00")
	index, chosen, chosenCount := 0, -1, 0
	for pos := 0; pos < len(fields); {
		header := strings.Fields(fields[pos])
		pos++
		if len(header) != 5 || !strings.HasPrefix(header[0], ":") || pos >= len(fields) {
			return "", errors.New("invalid Git raw change record")
		}
		status := header[4][0]
		path, old := fields[pos], ""
		pos++
		if status == 'R' || status == 'C' {
			if pos >= len(fields) {
				return "", errors.New("missing Git rename destination")
			}
			old, path = path, fields[pos]
			pos++
		}
		blocks := 1
		if status == 'T' {
			blocks = 2
		} // Git emits deletion and creation patches for type changes.
		if path == selected.Path && old == selected.OldPath {
			chosen, chosenCount = index, blocks
		}
		index += blocks
	}
	if !strings.HasPrefix(patch, "diff --git ") {
		return "", errors.New("missing Git patch header")
	}
	patches := strings.Split(patch, "\ndiff --git ")
	if len(patches) != index || chosen < 0 {
		return "", errors.New("Git patch records do not match selected change")
	}
	var result strings.Builder
	for block := chosen; block < chosen+chosenCount; block++ {
		piece := patches[block]
		if block > 0 {
			piece = "diff --git " + piece
		}
		if !strings.HasSuffix(piece, "\n") {
			piece += "\n"
		}
		result.WriteString(piece)
	}
	return result.String(), nil
}
