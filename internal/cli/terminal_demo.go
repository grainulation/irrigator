package cli

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/grainulation/irrigator/examples"
	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/commandflags"
	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runops"
	"github.com/grainulation/irrigator/internal/session"
)

var terminalDemoPlan = examples.TerminalSwarmJSON

// terminalDemo exercises real scheduling, child expansion and persistence for free.
func terminalDemo(args []string) (int, error) {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	target := fs.String("run", "", "new run directory; defaults inside the launch-context workspace or a retained temporary directory")
	registry := fs.String("session-registry", "", "shared session registry path; inherits IRRIGATOR_SESSION_REGISTRY; omitted headless demos do not publish")
	schedule := fs.String("schedule", runops.ScheduleBreadth, "demo ready order: breadth (demo default) or depth")
	if err := commandflags.ParseCommandFlags(fs, args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0, nil
		}
		return 2, err
	}
	if fs.NArg() != 0 {
		return 2, errors.New("demo accepts flags only; use --run DIR and optional --session-registry FILE")
	}
	order, err := runops.NormalizeSchedulingOrder(*schedule)
	if err != nil || order == "" {
		return 2, errors.New("demo --schedule must be depth or breadth")
	}
	abs, selectedRegistry, err := terminalDemoDestination(*target, *registry)
	if err != nil {
		return 1, err
	}
	oldRegistry := session.ActiveSessionRegistry
	session.ActiveSessionRegistry = selectedRegistry
	defer func() { session.ActiveSessionRegistry = oldRegistry }()
	var plan model.Plan
	if err = json.Unmarshal(terminalDemoPlan, &plan); err != nil {
		return 1, err
	}
	if err = checkpoint.Initialize(plan, abs, "mock"); err != nil {
		return 1, err
	}
	fmt.Println("Irrigator live swarm: real scheduling and child graphs, no model calls.")
	oldOrder := runops.ActiveSchedulingOrder
	runops.ActiveSchedulingOrder = order
	defer func() { runops.ActiveSchedulingOrder = oldOrder }()
	fmt.Printf("Scheduling order: %s (explicit demo selection; apply/resume default stays depth)\n", order)
	code, err := runops.Run(abs, model.Options{Workers: 4, Timeout: 15 * time.Second, Budget: .01})
	fmt.Printf("Demo state retained: %s\n", abs)
	binary := demoShellArg(os.Args[0])
	runArg := demoShellArg(abs)
	fmt.Printf("Inspect this run:\n  %s status %s\n  %s inspect %s discover\n", binary, runArg, binary, runArg)
	if code == 0 && err == nil {
		fmt.Printf("Create your own plan (use new filenames and run directories):\n  %s init --out plan.json\n  %s plan plan.json\n  %s apply plan.json --run runs/first --backend mock\n", binary, binary, binary)
	}
	return code, err
}

// Printed commands must preserve literal paths when pasted into a POSIX shell.
func demoShellArg(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

// Validate routing and launch confinement before creating any run directory.
// Headless demos default to no project registry.
func terminalDemoDestination(target, registryFlag string) (string, string, error) {
	var capsule *model.LaunchCapsule
	var err error
	if name := os.Getenv("IRRIGATOR_LAUNCH_CONTEXT"); name != "" {
		capsule, err = launch.ReadLaunchCapsule(name)
		if err != nil {
			return "", "", err
		}
	}
	registry := ""
	if registryFlag != "" || os.Getenv("IRRIGATOR_SESSION_REGISTRY") != "" || capsule != nil {
		registry, err = session.ResolveRegistryPath(registryFlag)
		if err != nil {
			return "", "", err
		}
	}
	if target == "" && capsule != nil {
		nonce := make([]byte, 8)
		if _, err = rand.Read(nonce); err != nil {
			return "", "", err
		}
		target = filepath.Join(capsule.Workspace, "runs", "demo-"+hex.EncodeToString(nonce))
	}
	if target != "" {
		target, err = filepath.Abs(target)
		if err != nil {
			return "", "", err
		}
	}
	if capsule != nil {
		c, err := launch.CurrentLaunchCapsule(target)
		if err != nil {
			return "", "", err
		}
		if c == nil || registry != c.Registry {
			return "", "", errors.New("selected registry conflicts with launch context")
		}
	}
	if target == "" {
		dir, err := os.MkdirTemp("", "irrigator-demo-")
		if err != nil {
			return "", "", err
		}
		target = filepath.Join(dir, "run")
	}
	return target, registry, nil
}
