package campaign

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/commandflags"
	"github.com/grainulation/irrigator/internal/jsonio"
	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/model"
)

func CampaignInit(dir, budget string) error {
	amount, e := launch.ExecutionBudgetMicros(budget)
	if e != nil {
		return e
	}
	dir, e = checkpoint.CampaignCanonical(dir)
	if e != nil {
		return e
	}
	if e = os.Mkdir(dir, 0700); e != nil {
		return e
	}
	c, e := checkpoint.OpenCampaign(dir)
	if e != nil {
		return e
	}
	defer c.Close()
	id, e := checkpoint.CampaignNonce()
	if e != nil {
		return e
	}
	return c.Write(checkpoint.CampaignLedger{Schema: "irrigator.campaign/v1", ID: id, CeilingMicros: amount, Grants: map[string]checkpoint.CampaignGrant{}})
}
func CampaignAdmit(dir string, p model.Plan, runDir string) (checkpoint.CampaignGrant, error) {
	var empty checkpoint.CampaignGrant
	if err := checkpoint.ValidateChannelPublishAdmission(p); err != nil {
		return empty, err
	}
	if checkpoint.PlanTracksCost(p) {
		return empty, errors.New("campaigns enforce shared budgets; use cost_policy retain_unknown for campaign admission")
	}
	if p.Campaign != nil || (p.APIVersion != launch.UnknownCostPlanVersion && !launch.SupportsGovernedPlan(p.APIVersion)) {
		return empty, errors.New("campaign admission requires an unbound swarm/v4 or swarm/v5 plan")
	}
	if e := checkpoint.ValidatePlanContext(p, true); e != nil {
		return empty, e
	}
	if e := checkpoint.ValidateInitialTurns(p); e != nil {
		return empty, e
	}
	if e := checkpoint.ValidateObserverShape(p); e != nil {
		return empty, e
	}
	var e error
	dir, e = checkpoint.CampaignCanonical(dir)
	if e != nil {
		return empty, e
	}
	runDir, e = checkpoint.CampaignCanonical(runDir)
	if e != nil {
		return empty, e
	}
	if e = checkpoint.RejectQuarantinedRun(runDir); e != nil {
		return empty, e
	}
	if _, e = os.Lstat(runDir); !os.IsNotExist(e) {
		return empty, errors.New("campaign run destination must be new")
	}
	// Full adapter preflight occurs before reserving. Reverification at initialize
	// and dispatch can still fail; such failures intentionally retain the grant.
	if _, e = checkpoint.InitializeManagedExecution(p); e != nil {
		return empty, e
	}
	c, e := checkpoint.OpenCampaign(dir)
	if e != nil {
		return empty, e
	}
	ledger, e := c.Read()
	if e != nil {
		c.Close()
		return empty, e
	}
	if len(ledger.Grants) >= 1024 {
		c.Close()
		return empty, errors.New("campaign grant capacity exhausted")
	}
	for _, g := range ledger.Grants {
		if g.RunDirectory == runDir {
			c.Close()
			return empty, errors.New("run destination already reserved; inspect existing grant")
		}
	}
	id, e := checkpoint.CampaignNonce()
	if e != nil {
		c.Close()
		return empty, e
	}
	if p.APIVersion != launch.ArtifactPlanVersion {
		p.APIVersion = model.GovernedPlanVersion
	}
	p.Campaign = &model.CampaignBinding{Directory: dir, CampaignID: ledger.ID, GrantID: id}
	// Validate the real bound plan before persisting a reservation. The input
	// plan is a value copy; no placeholder authority or caller-plan mutation.
	if e = checkpoint.Validate(p); e != nil {
		c.Close()
		return empty, e
	}
	amount, _ := launch.ExecutionBudgetMicros(p.Execution.RunBudgetUSD)
	g := checkpoint.CampaignGrant{ID: id, RunDirectory: runDir, PlanHash: launch.ExecutionDigest(p), ReservedMicros: amount, Status: "reserved"}
	ledger.Grants[id] = g
	e = c.Write(ledger)
	c.Close()
	if e != nil {
		return empty, e
	}
	// Never roll back a persisted grant on failure: a crash must not create a
	// second allowance. Retrying this admission requires a new destination/grant.
	if e = checkpoint.Initialize(p, runDir, "managed"); e != nil {
		return g, fmt.Errorf("campaign grant %s retained after preparation failure: %w", id, e)
	}
	return g, nil
}

func CampaignClose(runDir string) error {
	if e := checkpoint.RejectQuarantinedRun(runDir); e != nil {
		return e
	}
	runDir, e := checkpoint.CampaignCanonical(runDir)
	if e != nil {
		return e
	}
	ctl, e := checkpoint.FileLock(filepath.Join(runDir, "controller.lock"), true)
	if e != nil {
		return errors.New("campaign close requires stopped controller")
	}
	defer checkpoint.Unlock(ctl)
	// Workers take lease then state.lock. Nonblocking leases avoid reversing that
	// order; controller exclusion prevents legitimate creation of new workers.
	snapshot, e := checkpoint.ReadState(runDir)
	if e != nil {
		return e
	}
	leases := []*os.File{}
	defer func() {
		for _, f := range leases {
			checkpoint.Unlock(f)
		}
	}()
	for id := range snapshot.Tasks {
		f, e := checkpoint.FileLock(filepath.Join(runDir, "cells", id, "lease"), true)
		if e != nil {
			return errors.New("campaign close requires stopped workers")
		}
		leases = append(leases, f)
	}
	lock, e := checkpoint.FileLock(filepath.Join(runDir, "state.lock"), false)
	if e != nil {
		return e
	}
	defer checkpoint.Unlock(lock)
	s, e := checkpoint.ReadState(runDir)
	if e != nil {
		return e
	}
	if len(s.Tasks) != len(snapshot.Tasks) {
		return errors.New("campaign task set changed during close")
	}
	for id := range s.Tasks {
		if snapshot.Tasks[id] == nil {
			return errors.New("campaign task set changed during close")
		}
	}
	if s.Mailbox != nil {
		if s.Mailbox.Cursor != len(s.Mailbox.Entries) {
			return errors.New("campaign close requires a drained mailbox")
		}
		for _, out := range s.Mailbox.Outbox {
			if !out.Acknowledged {
				return errors.New("campaign close requires acknowledged outbox receipts")
			}
		}
	}
	b := s.Plan.Campaign
	if b == nil {
		return errors.New("run has no campaign grant")
	}
	if e := checkpoint.ValidateBillingAuthority(s); e != nil {
		return e
	}
	var reported int64
	for id, t := range s.Tasks {
		if t.Status != "verified" && t.Status != "failed" && t.Status != "cancelled" {
			return errors.New("campaign close requires terminal work and reconciled execution")
		}
		u, e := launch.UnresolvedProcess(filepath.Join(runDir, "cells", id), t.Attempt)
		if e != nil || u {
			return errors.New("campaign close has unresolved process evidence")
		}
	}
	for _, r := range s.Execution.Reservations {
		amount, settled := checkpoint.ManagedAccountedAmount(s.Execution, r)
		if !settled {
			return errors.New("unknown or uncertain cost retains entire campaign grant")
		}
		reported += amount
	}
	c, e := checkpoint.OpenCampaign(b.Directory)
	if e != nil {
		return e
	}
	defer c.Close()
	l, e := c.Read()
	if e != nil {
		return e
	}
	g, ok := l.Grants[b.GrantID]
	if !ok || l.ID != b.CampaignID || g.RunDirectory != runDir || g.PlanHash != s.PlanHash {
		return errors.New("campaign grant identity mismatch")
	}
	if g.Status == "closed" {
		if g.ReportedMicros != reported {
			return errors.New("closed grant accounting changed")
		}
		return nil
	}
	if reported > g.ReservedMicros {
		return errors.New("reported overrun: retain campaign grant and reconcile")
	}
	g.Status = "closed"
	g.ReportedMicros = reported
	l.Grants[g.ID] = g
	return c.Write(l)
}
func CampaignCLI(args []string) (int, error) {
	if len(args) == 0 {
		return 2, errors.New("campaign init DIR --budget USD | admit DIR PLAN --run NEW | status DIR | close RUN")
	}
	f := flag.NewFlagSet("campaign", flag.ContinueOnError)
	budget := f.String("budget", "", "aggregate admission USD")
	run := f.String("run", "", "new run directory")
	if err := commandflags.CommandFlagMetadata(f, args[1:]); err != nil {
		return 2, err
	}
	// Existing CLI convention accepts positional arguments before flags.
	positional := []string{}
	flags := []string{}
	for i := 1; i < len(args); i++ {
		if args[i] == "--budget" || args[i] == "--run" {
			if i+1 >= len(args) {
				return 2, errors.New("missing campaign flag value")
			}
			flags = append(flags, args[i], args[i+1])
			i++
		} else {
			positional = append(positional, args[i])
		}
	}
	if e := f.Parse(flags); e != nil {
		return 2, e
	}
	switch args[0] {
	case "init":
		if len(positional) != 1 || *budget == "" || *run != "" {
			return 2, errors.New("campaign init DIR --budget USD")
		}
		return 0, CampaignInit(positional[0], *budget)
	case "admit":
		if len(positional) != 2 || *run == "" || *budget != "" {
			return 2, errors.New("campaign admit DIR PLAN --run NEW")
		}
		p, e := checkpoint.LoadCampaignPlan(positional[1])
		if e != nil {
			return 1, e
		}
		g, e := CampaignAdmit(positional[0], p, *run)
		if e != nil {
			return 1, e
		}
		return 0, jsonio.PrintJSON(g)
	case "status":
		if len(positional) != 1 || *run != "" || *budget != "" {
			return 2, errors.New("campaign status DIR")
		}
		c, e := checkpoint.OpenCampaign(positional[0])
		if e != nil {
			return 1, e
		}
		defer c.Close()
		l, e := c.Read()
		if e != nil {
			return 1, e
		}
		return 0, jsonio.PrintJSON(l)
	case "close":
		if len(positional) != 1 || *run != "" || *budget != "" {
			return 2, errors.New("campaign close RUN")
		}
		return 0, CampaignClose(positional[0])
	default:
		return 2, errors.New("unknown campaign command")
	}
}
