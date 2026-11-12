package graph

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/grainulation/irrigator/internal/jsonio"
)

const

// Bounded, deterministic, derived-only normalization of a spawn proposal. The
// original result bytes, expansion hash and costs are never touched: callers hash
// the original source and feed only the normalized form to strict decoding.
(
	childNormalizeMaxBytes = 1 << 20
)

// Strict allowlist of top-level child display metadata that may move into display.
var childDisplayKeys = [...]string{`title`, `why_spawned`, `done_when`, `outcome_field`}

// normalizeChildProposal accepts either {"tasks":[...]} or a bare JSON array and
// returns canonical {"tasks":[...]} JSON. It rejects duplicate keys (any depth),
// trailing data, unknown top-level keys, non-object children, oversize and deeply
// nested input. It performs no type coercion and no semantic repair.
func normalizeChildProposal(source string) (string, error) {
	if len(source) > childNormalizeMaxBytes {
		return ``, errors.New(`child proposal exceeds size limit`)
	}
	dec := json.NewDecoder(strings.NewReader(source))
	dec.UseNumber()
	value, err := jsonio.ParseStrictJSON(dec, 0)
	if err != nil {
		return ``, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return ``, errors.New(`child proposal must contain exactly one JSON value`)
	}
	var tasks []any
	switch top := value.(type) {
	case []any:
		tasks = top
	case map[string]any:
		if err := rejectFoldCollisions(top); err != nil {
			return ``, err
		}
		for key := range top {
			if key != `tasks` {
				return ``, fmt.Errorf(`unknown proposal key %q`, key)
			}
		}
		switch list := top[`tasks`].(type) {
		case nil:
		case []any:
			tasks = list
		default:
			return ``, errors.New(`tasks must be an array`)
		}
	default:
		return ``, errors.New(`child proposal must be a JSON array or object`)
	}
	out := make([]any, 0, len(tasks))
	for _, item := range tasks {
		child, ok := item.(map[string]any)
		if !ok {
			return ``, errors.New(`every child task must be a JSON object`)
		}
		if err := normalizeChildDisplay(child); err != nil {
			return ``, err
		}
		out = append(out, child)
	}
	encoded, err := json.Marshal(map[string]any{`tasks`: out})
	if err != nil {
		return ``, err
	}
	return string(encoded), nil
}

// normalizeChildDisplay moves allowlisted top-level metadata into display only
// when display lacks the key or holds an identical string; conflicts reject.
func normalizeChildDisplay(child map[string]any) error {
	if err := rejectFoldCollisions(child); err != nil {
		return err
	}
	var display map[string]any
	if raw, ok := child[`display`]; ok {
		var isObject bool
		if display, isObject = raw.(map[string]any); !isObject {
			return errors.New(`child display must be an object`)
		}
		if err := rejectFoldCollisions(display); err != nil {
			return err
		}
	}
	for _, key := range childDisplayKeys {
		raw, ok := child[key]
		if !ok {
			continue
		}
		text, isString := raw.(string)
		if !isString {
			return fmt.Errorf(`child %s must be a string`, key)
		}
		delete(child, key)
		if display == nil {
			display = map[string]any{}
		}
		if existing, has := display[key]; has {
			if old, isText := existing.(string); !isText || old != text {
				return fmt.Errorf(`child %s conflicts with display.%s`, key, key)
			}
			continue
		}
		display[key] = text
	}
	if display != nil {
		if err := rejectFoldCollisions(display); err != nil {
			return err
		}
		child[`display`] = display
	}
	return rejectFoldCollisions(child)
}

// rejectFoldCollisions refuses keys that differ only by case: Go's decoder would
// otherwise match both onto one struct field. Linear time.
func rejectFoldCollisions(m map[string]any) error {
	seen := make(map[string]bool, len(m))
	for key := range m {
		folded := strings.ToLower(key)
		if seen[folded] {
			return fmt.Errorf(`keys differing only by case: %q`, key)
		}
		seen[folded] = true
	}
	return nil
}

func childEffortRank(effort string) (int, bool) {
	switch effort {
	case `low`:
		return 1, true
	case `medium`:
		return 2, true
	case `high`:
		return 3, true
	}
	return 0, false
}

// childEffort resolves a child's effort. Omitted inherits the parent. An explicit
// recognized value lower than or equal to the parent's is kept; a higher or
// unrecognized value is rejected. An empty (unspecified) parent effort is treated
// conservatively: no explicit child effort is allowed, so nothing is escalated.
func childEffort(parent, child string) (string, error) {
	if child == `` {
		return parent, nil
	}
	childRank, ok := childEffortRank(child)
	if !ok {
		return ``, fmt.Errorf(`unsupported effort %s`, child)
	}
	if parent == `` {
		return ``, errors.New(`child cannot set effort when parent effort is unspecified`)
	}
	parentRank, ok := childEffortRank(parent)
	if !ok {
		return ``, fmt.Errorf(`unsupported parent effort %s`, parent)
	}
	if childRank > parentRank {
		return ``, errors.New(`child cannot increase model effort`)
	}
	return child, nil
}
