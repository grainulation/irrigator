package cli

import (
	"errors"
	"flag"
	"fmt"
	"sort"
	"strings"

	"github.com/grainulation/irrigator/internal/approvals"
	"github.com/grainulation/irrigator/internal/backup"
	"github.com/grainulation/irrigator/internal/branchreview"
	"github.com/grainulation/irrigator/internal/buildinfo"
	"github.com/grainulation/irrigator/internal/campaign"
	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/commandflags"
	"github.com/grainulation/irrigator/internal/composition"
	"github.com/grainulation/irrigator/internal/debug"
	"github.com/grainulation/irrigator/internal/executor"
	"github.com/grainulation/irrigator/internal/graph"
	"github.com/grainulation/irrigator/internal/jsonio"
	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/mcpserver"
	"github.com/grainulation/irrigator/internal/messaging"
	"github.com/grainulation/irrigator/internal/provider"
	"github.com/grainulation/irrigator/internal/selfupdate"
	"github.com/grainulation/irrigator/internal/session"
	"github.com/grainulation/irrigator/internal/telemetry"
)

type commandEntry struct {
	run              func([]string) (int, error)
	probe            []string
	additionalProbes [][]string
	subcommands      []string
	Hidden           bool
}

// This registry routes real CLI commands as well as metadata requests. Flags are
// never copied here: the command's own FlagSet supplies every definition.
func commands() map[string]commandEntry {
	commands := map[string]commandEntry{}
	for _, name := range strings.Fields("init plan prepare apply deliver resume retire status cost inspect graph events artifacts propose decisions policy logs pause down cancel retry resolve export gc size") {
		name := name
		commands[name] = commandEntry{run: func(args []string) (int, error) { return builtinCLI(append([]string{name}, args...)) }, Hidden: name == "events" || name == "artifacts"}
	}
	commands["_worker"] = commandEntry{run: func(args []string) (int, error) { return builtinCLI(append([]string{"_worker"}, args...)) }, Hidden: true}
	commands["_supervise"] = commandEntry{run: func([]string) (int, error) { return launch.Supervise(), nil }, Hidden: true}
	commands["help"] = commandEntry{run: helpCLI}
	commands["schema"] = commandEntry{run: checkpoint.SchemaCLI}
	commands["contracts"] = commandEntry{run: runtimeContractsCLI, probe: []string{"plan"}, additionalProbes: [][]string{{"receipt"}, {"git"}}, subcommands: []string{"plan", "receipt", "git"}}
	commands["billing"] = commandEntry{run: provider.BillingCLI, subcommands: []string{"reconcile"}}
	commands["campaign"] = commandEntry{run: campaign.CampaignCLI, probe: []string{"init"}, subcommands: []string{"init", "admit", "status", "close"}}
	commands["mailbox"] = commandEntry{run: messaging.MailboxCLI, subcommands: []string{"enqueue", "process", "outbox", "ack"}}
	commands["approvals"] = commandEntry{run: approvals.ApprovalsCLI, probe: []string{"link"}, additionalProbes: [][]string{{"serve"}, {"notify"}}, subcommands: []string{"link", "serve", "notify"}}
	commands["otel"] = commandEntry{run: telemetry.OtelCLI, probe: []string{"export"}, subcommands: []string{"export"}}
	commands["recover"] = commandEntry{run: backup.RecoveryCLI}
	commands["backup"] = commandEntry{run: backup.BackupCLI, probe: []string{"create"}, subcommands: []string{"create", "verify", "restore"}}
	commands["compose"] = commandEntry{run: composition.CompositionCLI}
	commands["update-check"] = commandEntry{run: selfupdate.UpdateCheckCLI}
	commands["profiles"] = commandEntry{run: executionProfilesCLI, probe: []string{"inspect"}, additionalProbes: [][]string{{"migrate-prepare"}, {"migrate-apply"}}, subcommands: []string{"inspect", "resolve", "migrate-prepare", "migrate-apply"}}
	commands["context"] = commandEntry{run: launch.LaunchContextCLI, probe: []string{"inspect"}, subcommands: []string{"create", "inspect"}}
	commands["helpers"] = commandEntry{run: graph.TurnHelpersCLI, probe: []string{"METADATA_ONLY", "METADATA_ONLY"}}
	commands["turn-shape"] = commandEntry{run: graph.TurnShapeCLI}
	commands["debug"] = commandEntry{run: debug.DebugCLI, additionalProbes: [][]string{{"fixture", "export"}}, subcommands: []string{"fixture"}}
	commands["delivery-status"] = commandEntry{run: graph.DeliveryStatusCLI}
	commands["agents"] = commandEntry{run: executor.AgentsCLI}
	commands["experiments"] = commandEntry{run: messaging.ExperimentsCLI}
	commands["channel"] = commandEntry{run: messaging.ChannelCLI, probe: []string{"list"}, additionalProbes: [][]string{{"approve"}}, subcommands: []string{"list", "approve"}}
	commands["session"] = commandEntry{run: session.SessionCLI, probe: []string{"list"}, additionalProbes: [][]string{{"show"}, {"delete"}, {"enable"}, {"disable"}, {"record"}}, subcommands: []string{"list", "show", "delete", "enable", "disable", "record"}}
	commands["review"] = commandEntry{run: branchreview.ReviewCLI, probe: []string{"show"}, additionalProbes: [][]string{{"list"}, {"change"}, {"diff"}, {"viewed"}, {"unviewed"}, {"approve"}, {"request-changes"}, {"prune"}, {"tui"}, {"capture"}, {"request"}, {"comment"}, {"feedback"}}, subcommands: []string{"list", "change", "diff", "viewed", "unviewed", "approve", "request-changes", "prune", "tui", "capture", "request", "show", "comment", "feedback"}}
	commands["demo"] = commandEntry{run: terminalDemo}
	commands["cleanup"] = commandEntry{run: executor.CleanupCLI}
	commands["mcp"] = commandEntry{run: mcpserver.McpCLI}
	for _, name := range []string{"serve", "connect"} {
		name := name
		commands[name] = commandEntry{run: func(args []string) (int, error) { return provider.ProviderCLI(append([]string{name}, args...)) }}
	}
	commands["version"] = commandEntry{run: func(args []string) (int, error) {
		if err := commandflags.CommandFlagMetadata(flag.NewFlagSet("version", flag.ContinueOnError), args); err != nil {
			return 2, err
		}
		fmt.Println(buildinfo.Version)
		return 0, nil
	}}
	commands["doctor"] = commandEntry{run: func(args []string) (int, error) {
		if err := commandflags.CommandFlagMetadata(flag.NewFlagSet("doctor", flag.ContinueOnError), args); err != nil {
			return 2, err
		}
		report := executor.DoctorReport()
		report["executors"] = executor.DoctorExecutors()
		report["irrigator_version"] = buildinfo.Version
		report["swarm_version"] = buildinfo.Version
		return 0, jsonio.PrintJSON(report)
	}}
	return commands
}
func commandDefinitions(selected []string) ([]commandflags.CommandDefinition, error) {
	entries := commands()
	if len(selected) > 1 {
		return nil, errors.New("usage: irrigator help --json [COMMAND]")
	}
	names := []string{}
	for name := range entries {
		if !strings.HasPrefix(name, "_") && (len(selected) == 0 || name == selected[0]) {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return nil, errors.New("unknown public command for help metadata")
	}
	sort.Strings(names)
	definitions := []commandflags.CommandDefinition{}
	for _, name := range names {
		entry := entries[name]
		// The v1 contract exposes a union of flags across subcommands. Probe
		// each distinct parser so metadata remains sourced from real FlagSets.
		flags := map[string]commandflags.CommandFlag{}
		for _, probe := range append([][]string{entry.probe}, entry.additionalProbes...) {
			args := append(append([]string{}, probe...), commandflags.MetadataArgument)
			_, err := entry.run(args)
			var metadata *commandflags.CommandMetadata
			if !errors.As(err, &metadata) {
				return nil, fmt.Errorf("%s did not return command metadata: %v", name, err)
			}
			for _, f := range metadata.Flags {
				if _, exists := flags[f.Name]; !exists {
					flags[f.Name] = f
				}
			}
		}
		merged := []commandflags.CommandFlag{}
		for _, f := range flags {
			merged = append(merged, f)
		}
		sort.Slice(merged, func(i, j int) bool { return merged[i].Name < merged[j].Name })
		definitions = append(definitions, commandflags.CommandDefinition{Name: name, Flags: merged, Subcommands: entry.subcommands, Hidden: entry.Hidden})
	}
	return definitions, nil
}
func commandHelpJSON(args []string) (int, error) {
	definitions, err := commandDefinitions(args)
	if err != nil {
		return 2, err
	}
	return 0, jsonio.PrintJSON(struct {
		Schema   string                           `json:"schema"`
		Version  string                           `json:"version"`
		Commands []commandflags.CommandDefinition `json:"commands"`
	}{"irrigator.cli/v1", buildinfo.Version, definitions})
}

func helpCLI(args []string) (int, error) {
	fs := flag.NewFlagSet("help", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "machine-readable command and flag definitions")
	if err := commandflags.ParseCommandFlags(fs, args); err != nil {
		return 2, err
	}
	if *asJSON {
		return commandHelpJSON(fs.Args())
	}
	fmt.Print(Usage)
	return 0, nil
}
