package cli

import (
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"strings"
	"time"

	"github.com/grainulation/irrigator/internal/approvals"
	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/commandflags"
	"github.com/grainulation/irrigator/internal/delivery"
	"github.com/grainulation/irrigator/internal/executor"
	"github.com/grainulation/irrigator/internal/jsonio"
	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/provider"
	"github.com/grainulation/irrigator/internal/runops"
	"github.com/grainulation/irrigator/internal/session"
	"github.com/grainulation/irrigator/internal/sizing"
)

const Usage = `Irrigator — durable local agent graphs

  billing reconcile RUN RECEIPT --attest-external-billing  Record externally verified cost
  campaign init|admit|status|close          Reserve admission across managed runs
  experiments [--json]                    List opt-in experiments and their risks
  channel list|approve CAMPAIGN CHANNEL    Inspect or approve verified cross-run notes
  mailbox enqueue|process|outbox|ack           Durable fenced local cell commands
  approvals link|serve|notify --run DIR    Decide a blocked request from a phone via a single-use link
  contracts plan FILE|receipt RUN         Inspect runtime guarantees or export redacted run evidence
  recover --run RUN --in FILE --out NEW_DIRECTORY  Exclusive resumable transfer
  backup create|verify|restore             Private quiescent evidence snapshots
  compose CONFIG --out PLAN                Compile pinned reusable workflows offline
  init --out plan.json                    Create a starter plan
  update-check --manifest FILE --signature FILE --artifact FILE --public-key FILE --installed-sequence N  Verify an update offline
  profiles inspect|resolve --catalog FILE  Inspect or resolve offline execution profiles
  profiles migrate-prepare|migrate-apply    Review and approve a stopped-run provider migration
  context create --workspace DIR --out FILE [--registry FILE]  Create validated launch context
  turn-shape XxYxZ [--json]              Preview turn/helper capacity; does not launch
  demo [--run DIR]                        Watch a credential-free animated swarm
  code --task ID=PROMPT [--agent claude|codex|agy]  Run coding agents in git worktrees; results land as review branches
  code cleanup --repo DIR --run RUNID     Remove a code run's worktrees (and, with --delete-branches, branches)
  schema                                Print the editor JSON Schema for plans
  plan PLAN [--json]                      Validate and preview dependencies
  prepare PLAN --run DIR                  Save a run without starting workers
  apply PLAN --run DIR [--workers 2]       Create and execute a run (PLAN may be -)
  deliver PLAN --run DIR --dir NEW_DIR --nodes IDS  Execute, assemble, and check
  resume DIR [--retry-failed]             Recover and continue unfinished work
  retire RUN_ID --reason TEXT [--superseded-by RUN_ID] [--session-registry REG]  Explicitly retire a stopped registered run (registry only)
  status DIR [--json] [--watch]           Inspect overall state
  cost RUN [--json]                       Cost statement per cell with totals
  inspect DIR NODE                       Inspect a node, operation, and attempts
  graph DIR                              Print dependency/ownership Mermaid graph
  helpers DIR NODE --operation OP --attempt N --request FILE  Admit bounded helpers during a running turn
  propose DIR FILE                       Queue a bounded graph revision proposal
  decisions DIR                          Inspect adaptive decisions and revisions
  policy DIR FILE                        Set validated graph adaptation bounds
  logs DIR [--follow] [--since N]          Read sequenced events
  otel export RUN [--follow] [--since N] [--endpoint URL]  Send run events to an OpenTelemetry collector
  debug DIR [--json] [--node ID]          Read-only run diagnosis; no model call
  debug fixture export DIR --out FILE     (experimental) Write a redacted replay fixture of a run
  review [list|change|diff|approve|...]    Review local changes, plans and decisions before pushing
  session [list|show|enable]              Big-picture feed of agent sessions (prompts, tool activity, replies)
  mcp [--runs DIR]... [--workspace DIR] [--allow-control]  Serve runs to MCP hosts over stdio (read-only unless allowed)
  agents                                  Which agents can be the main agent or swarm workers here
  delivery-status RUN_DIR                 Read-only delivery stage/acceptance/recovery; no model call
  pause DIR | down DIR                    Drain or stop compute; retain state
  cancel DIR NODE                         Cancel a node and its descendants
  retry DIR NODE [--recursive]            Retry failed/cancelled work explicitly
  resolve DIR NODE --result FILE          Reconcile an uncertain outcome
  export DIR [--out FILE | --dir NEW_DIR]  Export accepted artifacts
  gc DIR [--apply] [--keep 1]              Preview/delete obsolete attempt logs
  cleanup RUN [--dry-run] [--all-finished]  Remove finished herdr terminals and dead tmux sessions a run left
  serve --dir DIR [--workers 4 --budget 1] Reuse bounded local model sessions
  connect FILE                           Provider bridge using connection.json
  doctor | version                        Inspect the local installation

Also: irrigator size PLAN --sizing FILE [--budget USD --backend NAME] [--json] explains opt-in sizing; apply/resume --sizing enforces it.

Scheduling: apply/resume --schedule breadth is opt-in and starts ready plan roots before their descendants; default stays dependency-depth (docs/scheduling-order.md).

Use irrigator COMMAND --help for flags, or irrigator help --json [COMMAND] for machine-readable definitions. Runs and plans are ordinary local files.
`

func runCommand(args []string) (int, error) {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		fmt.Print(Usage)
		return 0, nil
	}
	entry, ok := commands()[args[0]]
	if !ok {
		return 2, fmt.Errorf("unknown command %q; use irrigator --help", args[0])
	}
	return entry.run(args[1:])
}

func builtinCLI(args []string) (int, error) {
	command := args[0]
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	var root, out, dir, resultPath, backend, taskFilter, sizingPath, legacyUnbudgeted string
	var contextFiles checkpoint.ContextImportFlags
	var managedInit bool
	var initProfile string
	var shape string
	var deliveryNodes, deliveryField, deliveryCheck string
	var expectedOperation string
	var retireReason, retireSuperseded string
	var recheck bool
	var schedule string
	checkTimeout := 60 * time.Second
	workers, budget, timeout := 2, .25, 120*time.Second
	var retry, asJSON, watch, follow, apply, recursive, allowUncertain, dryRun, confirmStopped bool
	since, keep := 0, 1
	interval := 250 * time.Millisecond
	expected := 1
	switch command {
	case "init":
		fs.BoolVar(&managedInit, "managed", false, "create a managed plan with an explicit local CLI profile")
		fs.StringVar(&initProfile, "profile", "", "managed profile selector: claude:MODEL or codex:MODEL")
		fs.StringVar(&out, "out", "irrigator.json", "new plan file")
		expected = 0
	case "deliver":
		fs.StringVar(&root, "run", "", "durable run directory; matching runs resume")
		fs.StringVar(&backend, "backend", "", "required for legacy plans: mock, haiku (paid), or named provider")
		fs.StringVar(&dir, "dir", "", "delivery directory")
		fs.StringVar(&deliveryNodes, "nodes", "", "comma-separated verified nodes to assemble")
		fs.StringVar(&deliveryField, "field", "files", "result field containing a JSON path-to-content map")
		fs.StringVar(&deliveryCheck, "check", "", "acceptance argv as JSON; runs in delivery directory")
		fs.DurationVar(&checkTimeout, "check-timeout", checkTimeout, "acceptance deadline")
		fs.BoolVar(&recheck, "recheck", false, "explicitly rerun failed or interrupted acceptance")
	case "apply", "prepare":
		fs.StringVar(&root, "run", "", "new run directory")
		fs.StringVar(&backend, "backend", "", "required for legacy plans: mock, haiku (paid), or named provider")
	case "resume":
		fs.BoolVar(&retry, "retry-failed", false, "retry replay-safe failed nodes")
		fs.StringVar(&legacyUnbudgeted, "legacy-unbudgeted", "", "allow unbudgeted legacy runs to proceed without budget enforcement ('allow')")
	case "_worker":
		expected = 2
		fs.StringVar(&expectedOperation, "operation", "", "controller admission fence")
	case "plan":
		fs.BoolVar(&asJSON, "json", false, "JSON plan")
		fs.Var(&contextFiles, "context-file", "import named UTF-8 context KEY=PATH (repeatable; requires --out)")
		fs.StringVar(&out, "out", "", "write a new validated plan without overwriting")
	case "size":
		fs.BoolVar(&asJSON, "json", false, "JSON report")
		fs.StringVar(&sizingPath, "sizing", "", "sizing.v1 calibration JSON")
		fs.StringVar(&backend, "backend", "haiku", "backend assumed for tasks without a provider (reservation estimate)")
		fs.Float64Var(&budget, "budget", budget, "per-call budget USD used for the reservation estimate")
		fs.StringVar(&shape, "shape", "", "AxDxS ownership shape (or WxD) to size without a PLAN")
	case "status":
		fs.BoolVar(&asJSON, "json", false, "JSON state")
		fs.BoolVar(&watch, "watch", false, "watch until run stops")
		fs.DurationVar(&interval, "interval", interval, "refresh interval")
	case "cost":
		fs.BoolVar(&asJSON, "json", false, "JSON cost statement")
	case "inspect":
		expected = 2
	case "propose", "policy":
		expected = 2
	case "decisions":
	case "graph", "events", "artifacts", "pause", "down":
	case "logs":
		fs.BoolVar(&follow, "follow", false, "follow events until interrupted")
		fs.IntVar(&since, "since", 0, "last event sequence already consumed")
		fs.StringVar(&taskFilter, "task", "", "node filter")
		fs.BoolVar(&asJSON, "json", false, "JSONL events")
	case "retire":
		fs.StringVar(&retireReason, "reason", "", "why the run is retired (required, 1-160 characters)")
		fs.StringVar(&retireSuperseded, "superseded-by", "", "optional registered, non-retired run that replaces this one")
		fs.StringVar(&retireSuperseded, "superseded_by", "", "alias of --superseded-by")
	case "cancel":
		expected = 2
	case "retry":
		expected = 2
		fs.BoolVar(&recursive, "recursive", false, "include descendants")
		fs.BoolVar(&allowUncertain, "allow-uncertain", false, "explicitly permit replay of uncertain external actions")
		fs.BoolVar(&confirmStopped, "confirm-stopped", false, "attest surviving provider/validator processes have been stopped")
	case "resolve":
		expected = 2
		fs.StringVar(&resultPath, "result", "", "JSON result reconciled from external system")
		fs.BoolVar(&confirmStopped, "confirm-stopped", false, "attest surviving provider/validator processes have been stopped")
	case "export":
		fs.StringVar(&out, "out", "", "new JSON bundle file")
		fs.StringVar(&dir, "dir", "", "new directory for mapped output files")
	case "gc":
		fs.BoolVar(&apply, "apply", false, "delete the listed obsolete files")
		fs.IntVar(&keep, "keep", 1, "raw attempts to retain per node")
		fs.BoolVar(&dryRun, "dry-run", false, "preview only (default)")
	default:
		return 2, fmt.Errorf("unknown command %q; use irrigator --help", command)
	}
	var sessionRegistry, executorFlag string
	if command == "apply" || command == "prepare" || command == "resume" || command == "deliver" {
		fs.StringVar(&executorFlag, "executor", "", "how workers are launched: "+strings.Join(executor.KnownExecutors(), " or ")+" (default: the run's recorded executor, then IRRIGATOR_EXECUTOR, then local)")
	}
	var notifyFlags approvals.RunNotifyFlags
	if command == "apply" || command == "resume" {
		fs.StringVar(&notifyFlags.Via, "notify", "", "notify a phone or webhook when the run blocks, completes or a cell becomes uncertain or failed: ntfy, webhook or off (default: the run's recorded choice, then IRRIGATOR_NOTIFY_VIA, then off)")
		fs.StringVar(&notifyFlags.Topic, "notify-topic", "", "ntfy topic for --notify ntfy (default IRRIGATOR_NTFY_TOPIC; the token is only read from IRRIGATOR_NTFY_TOKEN)")
		fs.StringVar(&notifyFlags.URL, "notify-url", "", "webhook URL for --notify webhook (default IRRIGATOR_APPROVALS_WEBHOOK)")
	}
	if command == "apply" || command == "resume" {
		fs.StringVar(&schedule, "schedule", "", "opt-in ready order: breadth (roots before descendants) or depth (default); omitted keeps the run's saved order")
		fs.StringVar(&sizingPath, "sizing", "", "opt-in sizing.v1 JSON enforced at admission; saved as RUN/sizing.json")
		fs.StringVar(&sessionRegistry, "session-registry", "", "absolute shared session registry v2 path (v1 migrates on next actual start; default ./.irrigator/current-session.json)")
	}
	if command == "retire" {
		fs.StringVar(&sessionRegistry, "session-registry", "", "absolute session registry path (same as apply/resume; default ./.irrigator/current-session.json)")
	}
	if command == "deliver" || command == "prepare" || command == "apply" || command == "resume" || command == "_worker" {
		fs.IntVar(&workers, "workers", workers, "maximum concurrent workers")
		fs.DurationVar(&timeout, "timeout", timeout, "timeout per call")
		fs.Float64Var(&budget, "call-budget", budget, "budget USD per call, not a graph-wide hard cap")
		fs.Float64Var(&budget, "budget", budget, "alias for --call-budget (per call, not graph-wide)")
	}
	if err := commandflags.CommandFlagMetadata(fs, args[1:]); err != nil {
		return 2, err
	}
	var flags, positional []string
	for i := 1; i < len(args); i++ {
		a := args[i]
		if a != "-" && strings.HasPrefix(a, "-") {
			flags = append(flags, a)
			name := strings.TrimLeft(strings.SplitN(a, "=", 2)[0], "-")
			f := fs.Lookup(name)
			isBool := false
			if f != nil {
				if b, ok := f.Value.(interface{ IsBoolFlag() bool }); ok {
					isBool = b.IsBoolFlag()
				}
			}
			if !strings.Contains(a, "=") && !isBool && name != "h" && name != "help" {
				if f == nil {
					return 2, fmt.Errorf("unknown flag %s", a)
				}
				if i+1 >= len(args) {
					return 2, fmt.Errorf("missing value for %s", a)
				}
				i++
				flags = append(flags, args[i])
			}
		} else {
			positional = append(positional, a)
		}
	}
	if err := fs.Parse(flags); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0, nil
		}
		return 2, err
	}
	explicitFlags := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { explicitFlags[f.Name] = true })
	if explicitFlags["budget"] && explicitFlags["call-budget"] {
		return 2, errors.New("choose --call-budget or its --budget alias, not both")
	}
	callBudgetSet := explicitFlags["budget"] || explicitFlags["call-budget"]
	if command == "size" && shape != "" {
		if len(positional) != 0 {
			return 2, errors.New("size: PLAN and --shape are mutually exclusive")
		}
		expected = 0
	}
	if len(positional) != expected {
		return 2, fmt.Errorf("%s expects %d positional arguments; use --help", command, expected)
	}
	if workers < 1 || workers > 128 || timeout <= 0 || budget <= 0 || math.IsNaN(budget) || math.IsInf(budget, 0) || interval < 20*time.Millisecond || since < 0 {
		return 2, errors.New("invalid limits: workers 1..128, positive timeout/budget, interval >=20ms, since >=0")
	}
	if timeout > executor.MaxCallTimeout {
		// Workers reject longer calls, so fail here instead of failing every cell.
		return 2, fmt.Errorf("--timeout %s exceeds the per-call maximum of %s", timeout, executor.MaxCallTimeout)
	}
	target := ""
	if len(positional) > 0 {
		target = positional[0]
	}
	scheduleOrder, schedErr := runops.NormalizeSchedulingOrder(schedule)
	if schedErr != nil {
		return 2, schedErr
	}
	sizing.ActiveSizingPath = sizingPath
	executor.ActiveExecutor = executorFlag
	approvals.ActiveNotify = notifyFlags
	if notifyFlags.Via != "" {
		if err := approvals.ValidNotifyVia(notifyFlags.Via, "--notify"); err != nil {
			return 2, err
		}
	}
	if executorFlag != "" {
		if err := executor.ValidateExecutorName(executorFlag, "--executor"); err != nil {
			return 2, err
		}
	}
	opts := model.Options{Workers: workers, Timeout: timeout, Budget: budget, Retry: retry}
	if os.Getenv("IRRIGATOR_LAUNCH_CONTEXT") != "" && (command == "apply" || command == "prepare" || command == "resume" || command == "deliver") {
		destination := root
		if command == "resume" {
			destination = target
		}
		if destination == "" {
			return 2, errors.New("run destination is required with launch context")
		}
		capsule, err := launch.CurrentLaunchCapsule(destination)
		if err != nil {
			return 1, err
		}
		registry, err := session.ResolveRegistryPath(sessionRegistry)
		if err != nil {
			return 1, err
		}
		if capsule == nil || registry != capsule.Registry {
			return 1, errors.New("selected registry conflicts with launch context")
		}
		session.ActiveSessionRegistry = registry
	}
	switch command {
	case "init":
		return cmdInit(out, initProfile, managedInit)
	case "plan":
		return cmdPlan(target, contextFiles, out, asJSON)
	case "size":
		if shape != "" {
			return sizing.SizeShapeCLI(shape, sizing.ActiveSizingPath, asJSON, budget, backend)
		}
		return sizing.SizeCLI(target, sizing.ActiveSizingPath, asJSON, budget, backend)
	case "deliver":
		if backend == "" {
			return 2, errors.New("--backend is required: use mock for a free run, or explicitly select haiku or a named provider")
		}
		return delivery.Deliver(target, root, backend, dir, deliveryNodes, deliveryField, deliveryCheck, checkTimeout, recheck, opts)
	case "retire":
		return cmdRetire(target, retireReason, retireSuperseded, sessionRegistry)
	case "apply", "prepare":
		return cmdApply(command, target, root, backend, executorFlag, sessionRegistry, scheduleOrder, opts, callBudgetSet, explicitFlags["backend"])
	case "resume":
		return cmdResume(target, sessionRegistry, scheduleOrder, legacyUnbudgeted, retry, opts, explicitFlags["timeout"], callBudgetSet)
	case "_worker":
		return 0, executor.WorkerOperation(target, positional[1], opts, expectedOperation)
	case "pause", "down":
		if err := checkpoint.SetControl(target, command); err != nil {
			return 1, err
		}
		fmt.Printf("%s requested; durable state retained\n", command)
		return 0, nil
	case "cancel":
		return cmdCancel(target, positional[1])
	case "retry":
		return cmdRetry(target, positional[1], recursive, allowUncertain, confirmStopped)
	case "resolve":
		return cmdResolve(target, positional[1], resultPath, confirmStopped)
	case "inspect":
		v, err := runops.InspectNode(target, positional[1])
		if err != nil {
			return 1, err
		}
		return 0, jsonio.PrintJSON(v)
	case "propose":
		return cmdPropose(target, positional[1])
	case "policy":
		return cmdPolicy(target, positional[1])
	case "decisions":
		return cmdDecisions(target)
	case "graph":
		s, err := checkpoint.ReadState(target)
		if err != nil {
			return 1, err
		}
		fmt.Print(runops.GraphMermaid(s))
		return 0, nil
	case "export":
		return cmdExport(target, out, dir)
	case "gc":
		return cmdGC(target, keep, apply, dryRun)
	case "logs":
		return cmdLogs(target, since, taskFilter, follow, asJSON)
	case "cost":
		return provider.CostStatement(target, asJSON)
	case "status", "events", "artifacts":
		return cmdStatus(command, target, watch, asJSON, interval)
	}
	return 0, nil
}
