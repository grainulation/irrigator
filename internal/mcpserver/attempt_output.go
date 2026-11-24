package mcpserver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/commandflags"
	"github.com/grainulation/irrigator/internal/jsonio"
	"github.com/grainulation/irrigator/internal/localfs"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/textsafe"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const attemptOutputSchema = "irrigator.attempt-output/v1"

var attemptOutputTaskID = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

type attemptOutputInput struct {
	Run      string `json:"run" jsonschema:"saved run directory or a run name under --runs"`
	Task     string `json:"task" jsonschema:"exact saved task ID"`
	Attempt  int    `json:"attempt" jsonschema:"explicit historical attempt number, at least 1"`
	Stream   string `json:"stream,omitempty" jsonschema:"stdout (default) or stderr; input and credential files cannot be read"`
	Cursor   string `json:"cursor,omitempty" jsonschema:"next_cursor returned by the previous page of this exact source snapshot"`
	MaxBytes int    `json:"max_bytes,omitempty" jsonschema:"presentation bytes per page, default 4096; 4 through 8192"`
}

type attemptOutputCursor struct {
	Schema string `json:"schema"`
	Source string `json:"source"`
	Offset int    `json:"offset"`
}

type attemptOutputPage struct {
	Schema          string `json:"schema"`
	RunID           string `json:"run_id"`
	Task            string `json:"task"`
	Attempt         int    `json:"attempt"`
	Stream          string `json:"stream"`
	Representation  string `json:"representation"`
	SourceSHA256    string `json:"source_sha256"`
	SourceBytes     int    `json:"source_bytes"`
	SourceTruncated *bool  `json:"source_truncated"`
	CaptureComplete *bool  `json:"capture_complete"`
	CaptureMetadata string `json:"capture_metadata"`
	Offset          int    `json:"offset"`
	NextOffset      int    `json:"next_offset"`
	TotalBytes      int    `json:"total_bytes"`
	HasMore         bool   `json:"has_more"`
	NextCursor      string `json:"next_cursor,omitempty"`
	Output          string `json:"output"`
}

// AttemptOutputCLI reads only an explicitly selected attempt's captured output.
// It never launches work or changes run, attempt or capture artifacts.
func AttemptOutputCLI(args []string) (int, error) {
	fs := flag.NewFlagSet("attempt-output", flag.ContinueOnError)
	var in attemptOutputInput
	fs.IntVar(&in.Attempt, "attempt", 0, "explicit historical attempt number (required)")
	fs.StringVar(&in.Stream, "stream", "stdout", "stdout or stderr")
	fs.StringVar(&in.Cursor, "cursor", "", "next_cursor from the same source snapshot")
	fs.IntVar(&in.MaxBytes, "max-bytes", 4096, "presentation bytes per page (4..8192)")
	if err := commandflags.ParseCommandArguments(fs, args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0, nil
		}
		return 2, err
	}
	if fs.NArg() != 2 {
		return 2, errors.New("usage: irrigator attempt-output RUN TASK --attempt N [--stream stdout|stderr] [--cursor CURSOR] [--max-bytes N]")
	}
	in.Run, in.Task = fs.Arg(0), fs.Arg(1)
	page, err := readAttemptOutput(in)
	if err != nil {
		return 1, err
	}
	return 0, jsonio.PrintJSON(page)
}

func (t *mcpTools) attemptOutput(_ context.Context, _ *mcp.CallToolRequest, in attemptOutputInput) (*mcp.CallToolResult, mcpOut, error) {
	root, err := t.resolveRun(in.Run)
	if err != nil {
		return nil, nil, err
	}
	in.Run = root
	page, err := readAttemptOutput(in)
	if err != nil {
		return nil, nil, err
	}
	out, err := mcpDiagnosticObject(page)
	if err != nil {
		return nil, nil, err
	}
	return mcpResult(fmt.Sprintf("%d presentation byte(s); more=%t; capture metadata=%s", page.NextOffset-page.Offset, page.HasMore, page.CaptureMetadata), out)
}

func readAttemptOutput(in attemptOutputInput) (attemptOutputPage, error) {
	var page attemptOutputPage
	if in.Attempt < 1 || !attemptOutputTaskID.MatchString(in.Task) {
		return page, errors.New("an exact task ID and positive attempt number are required")
	}
	if in.Stream == "" {
		in.Stream = "stdout"
	}
	suffix, sourceLimit := ".stdout.json", int64(model.MaxOutputBytes)
	switch in.Stream {
	case "stdout":
	case "stderr":
		suffix, sourceLimit = ".stderr.log", 1024*1024
	default:
		return page, errors.New("stream must be stdout or stderr")
	}
	if in.MaxBytes == 0 {
		in.MaxBytes = 4096
	}
	if in.MaxBytes < utf8.UTFMax || in.MaxBytes > McpTextCap {
		return page, errors.New("max_bytes must be within 4..8192")
	}
	root, err := localfs.Abs(in.Run)
	if err != nil {
		return page, errors.New("invalid run path")
	}
	// Use the regular validator on bytes read through the confined, no-symlink
	// opener. This avoids a path-based second open of state.json.
	stateBytes, err := readAttemptFile(filepath.Join(root, "state.json"), 64*1024*1024)
	if err != nil {
		return page, fmt.Errorf("cannot read run checkpoint: %w", err)
	}
	state, err := checkpoint.DecodeState(root, stateBytes)
	if err != nil {
		return page, err
	}
	maxAttempt := 0
	if row := state.Tasks[in.Task]; row != nil {
		maxAttempt = row.Attempt
	}
	for _, row := range state.ArchivedTasks {
		if row.Spec.ID == in.Task {
			maxAttempt = max(maxAttempt, row.Attempt)
		}
	}
	if in.Attempt > maxAttempt {
		return page, errors.New("task attempt is not recorded in this run")
	}
	prefix := filepath.Join(root, "cells", in.Task, strconv.Itoa(in.Attempt))
	raw, err := readAttemptFile(prefix+suffix, sourceLimit)
	if err != nil {
		return page, fmt.Errorf("captured %s is unavailable for this attempt: %w", in.Stream, err)
	}
	sourceDigest := sha256.Sum256(raw)
	text := textsafe.Terminal(redactAttemptText(raw))
	truncated, complete, metadata := readCaptureMetadata(prefix, in.Stream, sourceDigest, len(raw), sourceLimit)
	// Bind pagination to this exact run location, attempt, stream, raw source,
	// redaction result and capture metadata. A changed/growing source requires a
	// fresh first page; a cursor can never select a different file or attempt.
	identityBytes, _ := json.Marshal([]any{root, state.RunID, in.Task, in.Attempt, in.Stream, hex.EncodeToString(sourceDigest[:]), text, truncated, complete, metadata})
	identity := sha256.Sum256(identityBytes)
	sourceID := hex.EncodeToString(identity[:])
	offset := 0
	if in.Cursor != "" {
		if len(in.Cursor) > 1024 {
			return page, errors.New("invalid output cursor")
		}
		decoded, err := base64.RawURLEncoding.DecodeString(in.Cursor)
		if err != nil {
			return page, errors.New("invalid output cursor")
		}
		var cursor attemptOutputCursor
		dec := json.NewDecoder(bytes.NewReader(decoded))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&cursor); err != nil || cursor.Schema != attemptOutputSchema || cursor.Source != sourceID || cursor.Offset < 0 || cursor.Offset > len(text) || (cursor.Offset < len(text) && !utf8.RuneStart(text[cursor.Offset])) {
			return page, errors.New("output cursor does not match this source snapshot; restart without cursor")
		}
		if dec.Decode(new(any)) != io.EOF {
			return page, errors.New("invalid output cursor")
		}
		offset = cursor.Offset
	}
	end := min(offset+in.MaxBytes, len(text))
	for end < len(text) && !utf8.RuneStart(text[end]) {
		end--
	}
	page = attemptOutputPage{
		Schema: attemptOutputSchema, RunID: state.RunID, Task: in.Task, Attempt: in.Attempt, Stream: in.Stream,
		Representation: "redacted-terminal-text", SourceSHA256: hex.EncodeToString(sourceDigest[:]), SourceBytes: len(raw),
		SourceTruncated: truncated, CaptureComplete: complete, CaptureMetadata: metadata,
		Offset: offset, NextOffset: end, TotalBytes: len(text), HasMore: end < len(text), Output: text[offset:end],
	}
	if page.HasMore {
		encoded, _ := json.Marshal(attemptOutputCursor{attemptOutputSchema, sourceID, end})
		page.NextCursor = base64.RawURLEncoding.EncodeToString(encoded)
	}
	return page, nil
}

func readAttemptFile(path string, limit int64) ([]byte, error) {
	f, err := localfs.OpenContextFile(path, os.O_RDONLY, 0)
	if err != nil {
		// Do not relay paths or operating-system diagnostics containing source
		// names supplied by an untrusted run.
		return nil, errors.New("missing, inaccessible or symlinked artifact")
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > limit {
		return nil, errors.New("artifact must be a regular file within its size bound")
	}
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(raw)) > limit {
		return nil, errors.New("artifact changed or exceeded its size bound")
	}
	return raw, nil
}

func readCaptureMetadata(prefix, stream string, digest [32]byte, size int, limit int64) (*bool, *bool, string) {
	raw, err := readAttemptFile(prefix+".capture.json", 4096)
	if err != nil {
		return nil, nil, "unrecorded"
	}
	var capture struct {
		Schema  string `json:"schema"`
		Streams map[string]struct {
			SHA256        string `json:"sha256"`
			CapturedBytes int64  `json:"captured_bytes"`
			LimitBytes    int64  `json:"limit_bytes"`
			Truncated     *bool  `json:"truncated"`
			Complete      *bool  `json:"complete"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(raw, &capture); err != nil || capture.Schema != "irrigator.process-capture/v1" {
		return nil, nil, "invalid"
	}
	item, ok := capture.Streams[stream]
	if !ok || item.Truncated == nil || item.Complete == nil {
		return nil, nil, "invalid"
	}
	if item.CapturedBytes != int64(size) || item.LimitBytes != limit || item.SHA256 != hex.EncodeToString(digest[:]) {
		return nil, nil, "mismatch"
	}
	if !*item.Complete && !*item.Truncated {
		return nil, item.Complete, "verified"
	}
	return item.Truncated, item.Complete, "verified"
}

var attemptBearer = regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/-]+=*`)
var attemptAssignments = regexp.MustCompile(`(?i)\b([A-Za-z_]*(?:token|secret|password|passwd|api[_-]?key|credential|authorization)[A-Za-z_]*)["']?\s*[:=]\s*(?:"(?:\\.|[^"\\])*"|'(?:\\.|[^'\\])*'|[^\s,;}]+)`)

// Redact the complete bounded snapshot before slicing it, so secrets and JSON
// credential fields cannot escape redaction by straddling page boundaries.
func redactAttemptText(raw []byte) string {
	var secrets []string
	for _, entry := range os.Environ() {
		key, val, _ := strings.Cut(entry, "=")
		if mcpSecretKey.MatchString(key) && len(val) >= 4 {
			secrets = append(secrets, val)
		}
	}
	sort.Slice(secrets, func(i, j int) bool { return len(secrets[i]) > len(secrets[j]) })
	redactKnown := func(text string) string {
		for _, secret := range secrets {
			text = strings.ReplaceAll(text, secret, "[redacted]")
		}
		return text
	}
	text := string(raw)
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err == nil && decoder.Decode(new(any)) == io.EOF {
		var walk func(any) any
		walk = func(v any) any {
			switch item := v.(type) {
			case string:
				// Redact decoded values before JSON escaping can hide known
				// secrets containing quotes, backslashes or HTML characters.
				return redactKnown(item)
			case map[string]any:
				for key, val := range item {
					switch {
					case strings.EqualFold(key, "env"):
						if env, ok := val.(map[string]any); ok {
							names := make([]string, 0, len(env))
							for name := range env {
								names = append(names, name)
							}
							sort.Strings(names)
							item[key] = names
						} else {
							item[key] = mcpEnvNames(val)
						}
					case mcpSecretKey.MatchString(key):
						item[key] = "[redacted]"
					default:
						item[key] = walk(val)
					}
				}
			case []any:
				for i := range item {
					item[i] = walk(item[i])
				}
			}
			return v
		}
		if encoded, err := json.Marshal(walk(value)); err == nil {
			text = string(encoded)
		}
	}
	// Object keys and JSON-lines fallback output can still contain encoded
	// secrets. Cover the canonical JSON spelling as well as decoded text.
	for _, secret := range secrets {
		encoded, _ := json.Marshal(secret)
		text = strings.ReplaceAll(text, string(encoded[1:len(encoded)-1]), "[redacted]")
	}
	text = redactKnown(text)
	text = attemptBearer.ReplaceAllString(text, "Bearer [redacted]")
	return attemptAssignments.ReplaceAllString(text, "$1=[redacted]")
}
