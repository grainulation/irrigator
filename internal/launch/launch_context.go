package launch

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"github.com/grainulation/irrigator/internal/buildinfo"
	"github.com/grainulation/irrigator/internal/commandflags"
	"github.com/grainulation/irrigator/internal/jsonio"
	"github.com/grainulation/irrigator/internal/model"
)

const LaunchContextSchema = "irrigator.launch/v1"
const WorkerContextSchema = "irrigator.context/v1"
const ContextReceiptSchema = "irrigator.context-receipt/v1"
const LaunchContextLimit = 16 * 1024

func ContextText(s string, max int) bool {
	return len(s) > 0 && len(s) <= max && !strings.ContainsFunc(s, unicode.IsControl)
}
func ContextPathInside(root, name string) bool {
	rel, err := filepath.Rel(root, name)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// Reject aliases through symlinks even when the destination has not been created.
func contextPath(name string) (string, error) {
	if !ContextText(name, 4096) || !filepath.IsAbs(name) || filepath.Clean(name) != name {
		return "", errors.New("context paths must be clean absolute paths")
	}
	for p := name; ; p = filepath.Dir(p) {
		info, err := os.Lstat(p)
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("context paths must not contain symlinks")
		}
		if err != nil && !os.IsNotExist(err) {
			return "", errors.New("context path cannot be inspected")
		}
		if p == filepath.Dir(p) {
			break
		}
	}
	return name, nil
}
func ExecutableDigest(name string) (string, error) {
	f, err := os.Open(name)
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 256*1024*1024 {
		return "", errors.New("runtime must be a bounded regular file")
	}
	h := sha256.New()
	if _, err = io.Copy(h, io.LimitReader(f, 256*1024*1024+1)); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func validateLaunchCapsule(c model.LaunchCapsule) error {
	if c.Schema != LaunchContextSchema || !ContextText(c.SessionID, 128) || !ContextText(c.RuntimeVersion, 128) || !c.HeadlessSupported {
		return errors.New("unsupported or malformed launch capsule")
	}
	if _, err := hex.DecodeString(c.RuntimeSHA256); err != nil || len(c.RuntimeSHA256) != 64 {
		return errors.New("invalid runtime digest")
	}
	if _, err := contextPath(c.Workspace); err != nil {
		return err
	}
	info, err := os.Stat(c.Workspace)
	if err != nil || !info.IsDir() {
		return errors.New("workspace must be an existing directory")
	}
	if _, err := contextPath(c.Registry); err != nil {
		return err
	}
	if !ContextPathInside(c.Workspace, c.Registry) || c.Registry == c.Workspace {
		return errors.New("registry must be inside the workspace")
	}
	return nil
}
func ReadLaunchCapsule(name string) (*model.LaunchCapsule, error) {
	if _, err := contextPath(name); err != nil {
		return nil, err
	}
	f, err := OpenContextFile(name, os.O_RDONLY, 0)
	if err != nil {
		return nil, fmt.Errorf("launch capsule cannot be opened safely: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > LaunchContextLimit {
		return nil, errors.New("launch capsule must be a bounded regular file")
	}
	raw, err := io.ReadAll(io.LimitReader(f, LaunchContextLimit+1))
	if err != nil || len(raw) > LaunchContextLimit {
		return nil, errors.New("launch capsule exceeds read limit")
	}
	strict := json.NewDecoder(bytes.NewReader(raw))
	parsed, err := jsonio.ParseStrictJSON(strict, 0)
	if err != nil {
		return nil, errors.New("invalid or duplicate launch capsule fields")
	}
	object, ok := parsed.(map[string]any)
	if !ok {
		return nil, errors.New("launch capsule must be an object")
	}
	allowed := map[string]bool{"schema": true, "session_id": true, "workspace": true, "registry": true, "runtime_version": true, "runtime_sha256": true, "headless_supported": true}
	for key := range object {
		if !allowed[key] {
			return nil, errors.New("unknown launch capsule field")
		}
	}
	var tail any
	if strict.Decode(&tail) != io.EOF {
		return nil, errors.New("launch capsule must contain exactly one object")
	}
	var c model.LaunchCapsule
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&c); err != nil {
		return nil, errors.New("invalid launch capsule JSON")
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		return nil, errors.New("launch capsule must contain exactly one object")
	}
	if err = validateLaunchCapsule(c); err != nil {
		return nil, err
	}
	return &c, nil
}
func CurrentLaunchCapsule(cell string) (*model.LaunchCapsule, error) {
	name := os.Getenv("IRRIGATOR_LAUNCH_CONTEXT")
	if name == "" {
		return nil, nil
	}
	c, err := ReadLaunchCapsule(name)
	if err != nil {
		return nil, err
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	hash, err := ExecutableDigest(exe)
	if err != nil || hash != c.RuntimeSHA256 || c.RuntimeVersion != buildinfo.Version {
		return nil, errors.New("launch capsule runtime identity mismatch")
	}
	absolute, err := filepath.Abs(cell)
	if err != nil {
		return nil, err
	}
	if _, err = contextPath(absolute); err != nil {
		return nil, err
	}
	if !ContextPathInside(c.Workspace, absolute) {
		return nil, errors.New("run destination is outside launch workspace")
	}
	if registry := os.Getenv("IRRIGATOR_SESSION_REGISTRY"); registry != "" && registry != c.Registry {
		return nil, errors.New("launch capsule registry conflicts with inherited registry")
	}
	return c, nil
}
func WorkContextDigest(c model.WorkerLaunchContext) string {
	c.Digest = ""
	raw, _ := json.Marshal(c)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// WorkRequest omits empty top-level Context and Dependencies maps on the wire.
// Canonicalize only those two maps; nested dependency maps remain significant
// because their null/object distinction survives JSON serialization.
func WorkerContextInputDigest(context map[string]string, dependencies map[string]map[string]string) string {
	if len(context) == 0 {
		context = nil
	}
	if len(dependencies) == 0 {
		dependencies = nil
	}
	return rawWorkerContextInputDigest(context, dependencies)
}

func rawWorkerContextInputDigest(context map[string]string, dependencies map[string]map[string]string) string {
	input, _ := json.Marshal(struct {
		Context      map[string]string
		Dependencies map[string]map[string]string
	}{context, dependencies})
	sum := sha256.Sum256(input)
	return hex.EncodeToString(sum[:])
}

func workerContextInputMatches(req model.WorkRequest, digest string) bool {
	if WorkerContextInputDigest(req.Context, req.Dependencies) == digest {
		return true
	}
	// Older envelopes hashed empty maps as {} before omitempty erased that
	// distinction. Accept only those wire-equivalent representations, without
	// rewriting the stored envelope/digest or relaxing any nonempty input.
	contexts := []map[string]string{req.Context}
	dependencies := []map[string]map[string]string{req.Dependencies}
	if len(req.Context) == 0 {
		contexts = []map[string]string{nil, {}}
	}
	if len(req.Dependencies) == 0 {
		dependencies = []map[string]map[string]string{nil, {}}
	}
	for _, context := range contexts {
		for _, dependency := range dependencies {
			if rawWorkerContextInputDigest(context, dependency) == digest {
				return true
			}
		}
	}
	return false
}

func PrepareWorkContext(req *model.WorkRequest, cell string) error {
	// Callers without an admitted run identity skip launch-context preparation.
	if req.RunID == "" || req.Attempt < 1 {
		if os.Getenv("IRRIGATOR_LAUNCH_CONTEXT") != "" {
			return errors.New("launch context requires admitted run identity")
		}
		return nil
	}
	if !ContextText(req.RunID, 128) || !ContextText(req.Task.ID, 256) || !ContextText(req.OperationID, 256) || !ContextText(req.RequestID, 256) || req.Attempt < 1 {
		return errors.New("invalid worker context identity")
	}
	capsule, err := CurrentLaunchCapsule(cell)
	if err != nil {
		return err
	}
	keys := make([]string, 0, len(req.Context))
	for k := range req.Context {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	c := model.WorkerLaunchContext{Schema: WorkerContextSchema, Capsule: capsule, RunID: req.RunID, TaskID: req.Task.ID, ParentID: req.Task.Parent, Turn: req.Task.Turn, RequestID: req.RequestID, OperationID: req.OperationID, Attempt: req.Attempt, Kind: req.Kind, ContextKeys: keys, InputSHA256: WorkerContextInputDigest(req.Context, req.Dependencies)}
	c.Digest = WorkContextDigest(c)
	req.LaunchContext = &c
	return nil
}
func ValidateWorkerLaunchContext(req model.WorkRequest) error {
	c := req.LaunchContext
	if c == nil {
		return nil
	}
	if c.Capsule != nil {
		if err := validateLaunchCapsule(*c.Capsule); err != nil {
			return err
		}
	}
	if c.Schema != WorkerContextSchema || c.RunID != req.RunID || c.TaskID != req.Task.ID || c.ParentID != req.Task.Parent || c.RequestID != req.RequestID || c.OperationID != req.OperationID || c.Attempt != req.Attempt || c.Kind != req.Kind || c.Digest != WorkContextDigest(*c) {
		return errors.New("worker context identity mismatch")
	}
	if !workerContextInputMatches(req, c.InputSHA256) {
		return errors.New("worker context input mismatch")
	}
	keys := make([]string, 0, len(req.Context))
	for k := range req.Context {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	a, _ := json.Marshal(keys)
	b, _ := json.Marshal(c.ContextKeys)
	if !bytes.Equal(a, b) {
		return errors.New("worker context selection mismatch")
	}
	a, _ = json.Marshal(req.Task.Turn)
	b, _ = json.Marshal(c.Turn)
	if !bytes.Equal(a, b) {
		return errors.New("worker context turn mismatch")
	}
	return nil
}

func RecordContextReceipt(prefix string, req model.WorkRequest, receipt *model.ContextReceipt) error {
	if receipt == nil {
		return nil
	} // No acknowledgement means no consumption claim.
	if req.LaunchContext == nil || receipt.Schema != ContextReceiptSchema || receipt.Digest != req.LaunchContext.Digest || !ContextText(receipt.Consumer, 128) {
		return errors.New("provider context receipt mismatch")
	}
	return jsonio.AtomicJSON(prefix+".context.json", struct {
		Schema      string                `json:"schema"`
		RunID       string                `json:"run_id"`
		TaskID      string                `json:"task_id"`
		OperationID string                `json:"operation_id"`
		Attempt     int                   `json:"attempt"`
		RequestID   string                `json:"request_id"`
		Evidence    string                `json:"evidence"`
		Receipt     *model.ContextReceipt `json:"receipt"`
	}{ContextReceiptSchema, req.RunID, req.Task.ID, req.OperationID, req.Attempt, req.RequestID, "provider_reported_consumption", receipt})
}
func LaunchContextCLI(args []string) (int, error) {
	if len(args) == 0 {
		return 2, errors.New("usage: irrigator context create --workspace PATH --out FILE [--registry PATH] | inspect --file FILE")
	}
	fs := flag.NewFlagSet("context "+args[0], flag.ContinueOnError)
	workspace := fs.String("workspace", "", "canonical workspace")
	registry := fs.String("registry", "", "workspace-contained registry")
	out := fs.String("out", "", "new capsule path")
	file := fs.String("file", "", "existing capsule path")
	if err := commandflags.ParseCommandFlags(fs, args[1:]); err != nil {
		return 2, err
	}
	if fs.NArg() != 0 {
		return 2, errors.New("unexpected context arguments")
	}
	switch args[0] {
	case "inspect":
		c, err := ReadLaunchCapsule(*file)
		if err != nil {
			return 1, err
		}
		return 0, jsonio.PrintJSON(c)
	case "create":
		exe, err := os.Executable()
		if err != nil {
			return 1, err
		}
		hash, err := ExecutableDigest(exe)
		if err != nil {
			return 1, err
		}
		if *registry == "" {
			*registry = filepath.Join(*workspace, ".irrigator", "current-session.json")
		}
		id := make([]byte, 16)
		if _, err = rand.Read(id); err != nil {
			return 1, err
		}
		c := model.LaunchCapsule{Schema: LaunchContextSchema, SessionID: hex.EncodeToString(id), Workspace: *workspace, Registry: *registry, RuntimeVersion: buildinfo.Version, RuntimeSHA256: hash, HeadlessSupported: true}
		if err = validateLaunchCapsule(c); err != nil {
			return 2, err
		}
		if _, err = contextPath(*out); err != nil {
			return 2, err
		}
		raw, err := json.MarshalIndent(c, "", "  ")
		if err != nil {
			return 1, err
		}
		f, err := OpenContextFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return 1, err
		}
		_, writeErr := f.Write(append(raw, '\n'))
		if writeErr == nil {
			writeErr = f.Sync()
		}
		closeErr := f.Close()
		if writeErr != nil {
			return 1, writeErr
		}
		if closeErr != nil {
			return 1, closeErr
		}
		fmt.Fprintln(os.Stdout, *out)
		return 0, nil
	default:
		return 2, errors.New("unknown context command")
	}
}
