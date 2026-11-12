package model

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Cost basis values.
const (
	CostBasisReported = "reported"
	CostBasisUnknown  = "unknown"
)

// TaskUsage records provider-reported token usage and model metadata.
type TaskUsage struct {
	InputTokens      int64  `json:"input_tokens,omitempty"`
	OutputTokens     int64  `json:"output_tokens,omitempty"`
	ThinkingTokens   int64  `json:"thinking_tokens,omitempty"`
	CacheReadTokens  int64  `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int64  `json:"cache_write_tokens,omitempty"`
	TotalTokens      int64  `json:"total_tokens,omitempty"`
	Model            string `json:"model,omitempty"`
}

// parseUsageNumber parses a non-negative integer token count.
func parseUsageNumber(v any) (int64, error) {
	switch n := v.(type) {
	case json.Number:
		s := n.String()
		if strings.ContainsAny(s, ".eE") {
			return 0, errors.New("token count must be an integer")
		}
		var val int64
		for _, r := range s {
			if r < '0' || r > '9' {
				return 0, errors.New("invalid token count")
			}
			val = val*10 + int64(r-'0')
			if val < 0 {
				return 0, errors.New("token count out of range")
			}
		}
		return val, nil
	case float64:
		if n < 0 || float64(int64(n)) != n {
			return 0, errors.New("token count must be a non-negative integer")
		}
		return int64(n), nil
	case int:
		if n < 0 {
			return 0, errors.New("token count must be non-negative")
		}
		return int64(n), nil
	case int64:
		if n < 0 {
			return 0, errors.New("token count must be non-negative")
		}
		return n, nil
	default:
		return 0, errors.New("token count must be a non-negative integer")
	}
}

// parseStrictUsage strictly validates a provider usage object.
// It ignores unknown keys, but strictly checks types of known token and model fields.
func ParseStrictUsage(raw any) (*TaskUsage, error) {
	if raw == nil {
		return nil, nil
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, errors.New("usage must be a JSON object")
	}
	u := &TaskUsage{}
	for k, v := range m {
		if v == nil {
			continue
		}
		switch k {
		case "input_tokens":
			n, err := parseUsageNumber(v)
			if err != nil {
				return nil, fmt.Errorf("invalid input_tokens: %w", err)
			}
			u.InputTokens = n
		case "output_tokens":
			n, err := parseUsageNumber(v)
			if err != nil {
				return nil, fmt.Errorf("invalid output_tokens: %w", err)
			}
			u.OutputTokens = n
		case "thinking_tokens":
			n, err := parseUsageNumber(v)
			if err != nil {
				return nil, fmt.Errorf("invalid thinking_tokens: %w", err)
			}
			u.ThinkingTokens = n
		case "cache_read_tokens":
			n, err := parseUsageNumber(v)
			if err != nil {
				return nil, fmt.Errorf("invalid cache_read_tokens: %w", err)
			}
			u.CacheReadTokens = n
		case "cache_write_tokens":
			n, err := parseUsageNumber(v)
			if err != nil {
				return nil, fmt.Errorf("invalid cache_write_tokens: %w", err)
			}
			u.CacheWriteTokens = n
		case "total_tokens":
			n, err := parseUsageNumber(v)
			if err != nil {
				return nil, fmt.Errorf("invalid total_tokens: %w", err)
			}
			u.TotalTokens = n
		case "model":
			s, ok := v.(string)
			if !ok {
				return nil, errors.New("model must be a string")
			}
			if !ValidUsageModel(s) {
				return nil, errors.New("invalid model string")
			}
			u.Model = s
		default:
			// Ignore unknown usage keys.
		}
	}
	// If total_tokens was not explicitly supplied, derive it from the parts.
	if u.TotalTokens == 0 {
		sum := u.InputTokens + u.OutputTokens + u.ThinkingTokens + u.CacheReadTokens + u.CacheWriteTokens
		if sum > 0 {
			u.TotalTokens = sum
		}
	}
	return u, nil
}

// validUsageModel validates an optional model string (short, printable utf8, no control chars).
func ValidUsageModel(s string) bool {
	return len(s) > 0 && len(s) <= 128 && utf8.ValidString(s) && !strings.ContainsFunc(s, unicode.IsControl)
}

// formatTokenSummary formats token usage for human presentation.
func FormatTokenSummary(u *TaskUsage) string {
	if u == nil {
		return ""
	}
	tot := u.TotalTokens
	if tot == 0 {
		tot = u.InputTokens + u.OutputTokens + u.ThinkingTokens + u.CacheReadTokens + u.CacheWriteTokens
	}
	if tot == 0 {
		return ""
	}
	var parts []string
	if u.InputTokens > 0 || u.OutputTokens > 0 {
		parts = append(parts, fmt.Sprintf("%d in", u.InputTokens))
		parts = append(parts, fmt.Sprintf("%d out", u.OutputTokens))
	}
	if u.ThinkingTokens > 0 {
		parts = append(parts, fmt.Sprintf("%d thinking", u.ThinkingTokens))
	}
	if u.CacheReadTokens > 0 {
		parts = append(parts, fmt.Sprintf("%d cached", u.CacheReadTokens))
	}
	if u.CacheWriteTokens > 0 {
		parts = append(parts, fmt.Sprintf("%d cache-write", u.CacheWriteTokens))
	}
	if len(parts) == 0 {
		return fmt.Sprintf("%d tokens", tot)
	}
	return fmt.Sprintf("%d tokens (%s)", tot, strings.Join(parts, ", "))
}

// addUsage aggregates token counts safely without integer overflow.
func AddUsage(dst, src *TaskUsage) *TaskUsage {
	if src == nil {
		return dst
	}
	if dst == nil {
		dst = &TaskUsage{}
	}
	dst.InputTokens += src.InputTokens
	dst.OutputTokens += src.OutputTokens
	dst.ThinkingTokens += src.ThinkingTokens
	dst.CacheReadTokens += src.CacheReadTokens
	dst.CacheWriteTokens += src.CacheWriteTokens
	tot := src.TotalTokens
	if tot == 0 {
		tot = src.InputTokens + src.OutputTokens + src.ThinkingTokens + src.CacheReadTokens + src.CacheWriteTokens
	}
	dst.TotalTokens += tot
	if dst.Model == "" && src.Model != "" {
		dst.Model = src.Model
	}
	return dst
}
