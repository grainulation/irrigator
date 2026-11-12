package jsonio

import (
	"encoding/json"
	"errors"
	"fmt"
)

const

// Bounded, deterministic, derived-only normalization of a spawn proposal. The
// original result bytes, expansion hash and costs are never touched: callers hash
// the original source and feed only the normalized form to strict decoding.
(
	childNormalizeMaxDepth = 32
)

// parseStrictJSON reads one value from dec, rejecting duplicate object keys and
// nesting deeper than childNormalizeMaxDepth. Numbers stay json.Number.
func ParseStrictJSON(dec *json.Decoder, depth int) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf(`invalid JSON: %w`, err)
	}
	delim, isDelim := tok.(json.Delim)
	if !isDelim {
		return tok, nil
	}
	if depth >= childNormalizeMaxDepth {
		return nil, errors.New(`child proposal nested too deeply`)
	}
	if delim == '{' {
		obj := map[string]any{}
		for dec.More() {
			keyTok, err := dec.Token()
			if err != nil {
				return nil, fmt.Errorf(`invalid JSON: %w`, err)
			}
			key, ok := keyTok.(string)
			if !ok {
				return nil, errors.New(`invalid JSON object key`)
			}
			if _, dup := obj[key]; dup {
				return nil, fmt.Errorf(`duplicate JSON key %q`, key)
			}
			val, err := ParseStrictJSON(dec, depth+1)
			if err != nil {
				return nil, err
			}
			obj[key] = val
		}
		if _, err := dec.Token(); err != nil {
			return nil, fmt.Errorf(`invalid JSON: %w`, err)
		}
		return obj, nil
	}
	list := []any{}
	for dec.More() {
		val, err := ParseStrictJSON(dec, depth+1)
		if err != nil {
			return nil, err
		}
		list = append(list, val)
	}
	if _, err := dec.Token(); err != nil {
		return nil, fmt.Errorf(`invalid JSON: %w`, err)
	}
	return list, nil
}
