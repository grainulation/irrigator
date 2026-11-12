package checkpoint

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/localfs"
	"github.com/grainulation/irrigator/internal/model"
)

const contextImportLimit = 1 << 20

var contextImportKey = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

type ContextImportFlags []string

func (f *ContextImportFlags) String() string         { return strings.Join(*f, ",") }
func (f *ContextImportFlags) Set(value string) error { *f = append(*f, value); return nil }

// This is an explicit offline materialization step, never a provider or resume
// operation. Imported contents are frozen in the new plan; paths are not stored.
func MaterializePlanContext(p *model.Plan, imports []string) error {
	if len(imports) == 0 {
		return nil
	}
	if len(imports) > 64 {
		return errors.New("at most 64 context files may be imported")
	}
	type entry struct{ key, path string }
	entries := make([]entry, 0, len(imports))
	keys := map[string]bool{}
	for _, spec := range imports {
		key, path, ok := strings.Cut(spec, "=")
		if !ok || path == "" {
			return errors.New("context file must be KEY=PATH with a nonempty path")
		}
		if !contextImportKey.MatchString(key) {
			return fmt.Errorf("invalid context key %q: use 1-128 letters, digits, dots, underscores or hyphens, starting with a letter or digit", key)
		}
		if _, exists := p.Context[key]; exists || keys[key] {
			return fmt.Errorf("context key %q already exists; choose a new key", key)
		}
		keys[key] = true
		entries = append(entries, entry{key, path})
	}
	additions := make(map[string]string, len(entries))
	total := 0
	for _, e := range entries {
		raw, err := readContextImport(e.path)
		if err != nil {
			return fmt.Errorf("context %s: %w", e.key, err)
		}
		total += len(raw)
		if total > 8<<20 {
			return errors.New("imported context exceeds 8 MiB")
		}
		additions[e.key] = string(raw)
	}
	if p.Context == nil {
		p.Context = map[string]string{}
	}
	for k, v := range additions {
		p.Context[k] = v
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	if len(raw) > 8<<20 {
		return errors.New("materialized plan exceeds 8 MiB")
	}
	return nil
}

func readContextImport(path string) ([]byte, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	// Resolve the explicitly selected parent (including macOS /var aliases), then
	// pin each real directory and reject a final symlink/FIFO/device before reading.
	parent, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	f, err := launch.OpenContextFile(filepath.Join(parent, filepath.Base(path)), os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() || st.Size() > contextImportLimit {
		return nil, errors.New("context file must be regular and at most 1 MiB")
	}
	raw, err := io.ReadAll(io.LimitReader(f, contextImportLimit+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > contextImportLimit {
		return nil, errors.New("context file exceeds 1 MiB")
	}
	if !utf8.Valid(raw) || strings.IndexByte(string(raw), 0) >= 0 {
		return nil, errors.New("context file must contain UTF-8 text without NUL bytes")
	}
	return raw, nil
}

func WriteContextPlan(out string, p model.Plan) error {
	raw, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	if len(raw)+1 > 8<<20 {
		return errors.New("materialized plan exceeds 8 MiB")
	}
	if err := RejectQuarantinedRun(filepath.Dir(out)); err != nil {
		return err
	}
	if err := localfs.WriteNew(out, append(raw, '\n')); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("plan output already exists: %s; choose a new --out path: %w", out, os.ErrExist)
		}
		return err
	}
	return nil
}
