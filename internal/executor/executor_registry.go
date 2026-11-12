package executor

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/grainulation/irrigator/internal/jsonio"
)

// A worker executor decides how the controller launches each `irrigator
// _worker`. The worker, its lease and its durable contract do not change with
// the executor; only where the process runs does.
//
// To add an executor (for example "tmux"): add one file, executor_<name>.go,
// with a type implementing workerExecutor and a constructor that checks its
// requirements (and fails with a clear message before any worker starts), then
// add one entry to workerExecutors below. Selection, the --executor flag, the
// per-run record and the unknown-name error need no other change. Document it
// in docs/executors.md and add an entry to doctorExecutors.
type workerExecutor interface {
	name() string
	// start launches `binary workerArgs...` for one cell. The caller Waits on
	// cmd, signals child, and calls done after Wait returns.
	Start(binary, root, runID, cell string, attempt int, workerArgs []string) (cmd *exec.Cmd, child ChildProcess, done func(), err error)
}

// workerExecutors maps executor names to constructors. A constructor is the
// preflight: it runs once per controller, before any worker is launched.
var workerExecutors = map[string]func() (workerExecutor, error){
	"local": func() (workerExecutor, error) { return localExecutor{}, nil },
	"herdr": newHerdrExecutor,
	"tmux":  newTmuxExecutor,
}

const DefaultExecutor = "local"

// ActiveExecutor is the explicit --executor flag for the controller this
// process starts; empty defers to the run record, IRRIGATOR_EXECUTOR, local.
var ActiveExecutor string

// childProcess is what the controller signals. *os.Process satisfies it.
type ChildProcess interface {
	Signal(os.Signal) error
	Kill() error
}

func KnownExecutors() []string {
	names := make([]string, 0, len(workerExecutors))
	for n := range workerExecutors {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func ValidateExecutorName(name, source string) error {
	if _, ok := workerExecutors[name]; ok {
		return nil
	}
	return fmt.Errorf("%s %q is not a known executor; use one of: %s", source, name, strings.Join(KnownExecutors(), ", "))
}

// localExecutor runs the worker as a direct child of the controller.
type localExecutor struct{}

func (localExecutor) name() string { return "local" }

func (localExecutor) Start(binary, root, runID, cell string, attempt int, workerArgs []string) (cmd *exec.Cmd, child ChildProcess, done func(), err error) {
	cmd = exec.Command(binary, workerArgs...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, nil, nil, err
	}
	return cmd, cmd.Process, func() {}, nil
}

// ExecutorRecordName is the file (RUN/executor.json) that records the executor
// chosen for a run, so a resume keeps using it. Runs without the file are
// readable and default to local.
const ExecutorRecordName = "executor.json"

type executorRecord struct {
	Executor string `json:"executor"`
}

func ReadExecutorRecord(root string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(root, ExecutorRecordName))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var rec executorRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return "", fmt.Errorf("unreadable %s: %w", ExecutorRecordName, err)
	}
	if err := ValidateExecutorName(rec.Executor, "recorded executor"); err != nil {
		return "", err
	}
	return rec.Executor, nil
}

func WriteExecutorRecord(root, name string) error {
	return jsonio.AtomicJSON(filepath.Join(root, ExecutorRecordName), executorRecord{Executor: name})
}

// chooseExecutor applies the precedence: explicit flag > the run's recorded
// executor > IRRIGATOR_EXECUTOR > local.
func chooseExecutor(root, flagValue string) (name, recorded string, err error) {
	recorded, err = ReadExecutorRecord(root)
	if err != nil {
		return "", "", err
	}
	switch {
	case flagValue != "":
		name = flagValue
		err = ValidateExecutorName(name, "--executor")
	case recorded != "":
		name = recorded
	case os.Getenv("IRRIGATOR_EXECUTOR") != "":
		name = os.Getenv("IRRIGATOR_EXECUTOR")
		err = ValidateExecutorName(name, "IRRIGATOR_EXECUTOR")
	default:
		name = DefaultExecutor
	}
	return name, recorded, err
}

// recordExecutorChoice persists the executor when a run is created (apply,
// prepare). An existing record is left alone.
func RecordExecutorChoice(root, flagValue string) error {
	name, recorded, err := chooseExecutor(root, flagValue)
	if err != nil {
		return err
	}
	if recorded != "" {
		return nil
	}
	return WriteExecutorRecord(root, name)
}

// resolveWorkerExecutor selects the executor for this controller, notes an
// override of the recorded executor, records the choice, and runs the
// executor's preflight.
func ResolveWorkerExecutor(root, flagValue string) (workerExecutor, error) {
	name, recorded, err := chooseExecutor(root, flagValue)
	if err != nil {
		return nil, err
	}
	ex, err := workerExecutors[name]()
	if err != nil {
		return nil, err
	}
	if recorded != name {
		if recorded != "" {
			fmt.Fprintf(os.Stderr, "executor: --executor %s overrides the run's recorded executor %s\n", name, recorded)
		}
		if err := WriteExecutorRecord(root, name); err != nil {
			return nil, err
		}
	}
	return ex, nil
}
