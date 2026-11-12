package backup

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/commandflags"
	"github.com/grainulation/irrigator/internal/jsonio"
	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/managed"
	"github.com/grainulation/irrigator/internal/model"
)

func RecoveryCLI(args []string) (int, error) {
	f := flag.NewFlagSet("recover", flag.ContinueOnError)
	source := f.String("run", "", "reachable quiescent source")
	bundle := f.String("in", "", "exact source backup")
	target := f.String("out", "", "new private target directory")
	version := f.Int("migrate-to", 0, "explicit checkpoint version (5 supports exact v4 migration)")
	if e := commandflags.ParseCommandFlags(f, args); e != nil {
		return 2, e
	}
	if f.NArg() != 0 || *source == "" || *bundle == "" || *target == "" {
		return 2, errors.New("recover requires --run SOURCE --in BACKUP --out NEW_DIRECTORY")
	}
	if e := RecoveryTransfer(*source, *bundle, *target, *version, nil); e != nil {
		return 1, e
	}
	return 0, jsonio.PrintJSON(map[string]any{"recovered": true, "target": *target, "uncertain_operations_replayed": false})
}

// hook is a crash-injection seam; each named stage is durable before it runs.
func RecoveryTransfer(source, bundle, target string, version int, hook func(string) error) error {
	source, e := filepath.Abs(source)
	if e != nil {
		return e
	}
	target, e = filepath.Abs(target)
	if e != nil {
		return e
	}
	if source == target || strings.HasPrefix(target, source+string(os.PathSeparator)) || strings.HasPrefix(source, target+string(os.PathSeparator)) {
		return errors.New("recovery directories must be disjoint")
	}
	m, e := BackupRead(bundle)
	if e != nil {
		return e
	}
	raw, e := checkpoint.BackupReadFile(bundle, backupLimit)
	if e != nil {
		return e
	}
	if version == 0 {
		version = m.StateVersion
	}
	if version != m.StateVersion && !(m.StateVersion == 4 && version == 5) {
		return errors.New("only explicit checkpoint v4 to v5 migration is supported")
	}
	sr, e := checkpoint.BackupOpenRoot(source)
	if e != nil {
		return fmt.Errorf("recovery requires reachable source: %w", e)
	}
	defer sr.Close()
	var locks []*os.File
	defer func() {
		for i := len(locks) - 1; i >= 0; i-- {
			checkpoint.Unlock(locks[i])
		}
	}()
	take := func(name string) error {
		f, e := launch.OpenContextFile(filepath.Join(source, name), os.O_CREATE|os.O_RDWR, 0600)
		if e != nil {
			return e
		}
		st, e := f.Stat()
		if e != nil || !st.Mode().IsRegular() || st.Sys().(*syscall.Stat_t).Nlink != 1 {
			f.Close()
			return errors.New("unsafe recovery lock")
		}
		if e = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
			f.Close()
			return errors.New("source active: recovery requires exclusive leases")
		}
		locks = append(locks, f)
		return nil
	}
	for _, p := range []string{"delivery.lock", "controller.lock", "state.lock"} {
		if e = take(p); e != nil {
			return e
		}
	}
	state, e := checkpoint.ReadState(source)
	if e != nil {
		return e
	}
	for _, t := range state.AllTasks() {
		if !RegistryRunID.MatchString(t.ID) {
			return errors.New("unsafe task ID")
		}
		if _, e = sr.Lstat(filepath.Join("cells", t.ID)); errors.Is(e, os.ErrNotExist) {
			continue
		}
		if e = take(filepath.Join("cells", t.ID, "lease")); e != nil {
			return e
		}
	}
	var original []byte
	for _, entry := range m.Files {
		if entry.Path == "state.json" {
			original = entry.Data
		}
		if strings.HasPrefix(entry.Path, "RECOVERY_") {
			return errors.New("recovery chains require a fresh native checkpoint; nested receipts unsupported")
		}
	}
	live, e := checkpoint.BackupReadFile(filepath.Join(source, "state.json"), checkpoint.BackupFileLimit)
	if e != nil {
		return e
	}
	if !bytes.Equal(original, live) {
		return errors.New("backup is not the exact source checkpoint")
	}
	inventory, e := BackupInventory(source)
	if e != nil {
		return e
	}
	filtered := inventory[:0]
	for _, entry := range inventory {
		if entry.Path != checkpoint.RecoverySource {
			filtered = append(filtered, entry)
		}
	}
	if !reflect.DeepEqual(filtered, m.Files) {
		return errors.New("backup source inventory changed")
	}
	var campaign *checkpoint.CampaignStore
	if state.Plan.Campaign != nil {
		campaign, e = checkpoint.OpenCampaign(state.Plan.Campaign.Directory)
		if e != nil {
			return e
		}
		defer campaign.Close()
		for _, name := range []string{"campaign.lock", "campaign.json"} {
			st, err := campaign.Root.Lstat(name)
			if err != nil {
				return err
			}
			stat, ok := st.Sys().(*syscall.Stat_t)
			if !st.Mode().IsRegular() || !ok || stat.Nlink != 1 {
				return errors.New("unsafe recovery campaign file")
			}
		}
	}
	r := checkpoint.RecoveryReceipt{Schema: "irrigator.recovery/v1", Source: source, Target: target, RunID: m.RunID, BundleSHA256: checkpoint.RecoveryDigest(raw), OriginalStateSHA256: checkpoint.RecoveryDigest(original), TargetVersion: version}
	prior, e := checkpoint.RecoveryRead(filepath.Join(source, checkpoint.RecoverySource))
	retry := e == nil
	if e != nil && !errors.Is(e, os.ErrNotExist) {
		return e
	}
	if !retry {
		if e = checkpoint.RejectQuarantinedRun(source); e != nil {
			return e
		}
		if e = checkpoint.RejectQuarantinedRun(filepath.Dir(target)); e != nil {
			return e
		}
	}
	if retry {
		r.Device = prior.Device
		r.Inode = prior.Inode
		if r != prior {
			return errors.New("source already bound to another recovery")
		}
	}
	if campaign != nil {
		l, err := campaign.Read()
		if err != nil {
			return err
		}
		g, ok := l.Grants[state.Plan.Campaign.GrantID]
		if !ok || g.PlanHash != state.PlanHash || (g.RunDirectory != source && (!retry || g.RunDirectory != target)) {
			return errors.New("campaign grant no longer belongs to source")
		}
		if g.RunDirectory == source {
			data, _ := json.Marshal(l)
			if m.Campaign == nil || !bytes.Equal(data, m.Campaign.Ledger.Data) {
				return errors.New("campaign changed since backup")
			}
		}
	}
	parent, e := checkpoint.BackupOpenRoot(filepath.Dir(target))
	if e != nil {
		return e
	}
	defer parent.Close()
	if !retry {
		if e = parent.Mkdir(filepath.Base(target), 0700); e != nil {
			return e
		}
	}
	pd, e := parent.Open(".")
	if e != nil {
		return e
	}
	e = pd.Sync()
	pd.Close()
	if e != nil {
		return e
	}
	tr, e := checkpoint.BackupOpenRoot(target)
	if e != nil {
		return e
	}
	defer tr.Close()
	st, e := tr.Stat(".")
	if e != nil {
		return e
	}
	ss := st.Sys().(*syscall.Stat_t)
	if retry && (r.Device != uint64(ss.Dev) || r.Inode != ss.Ino) {
		return errors.New("target directory replaced")
	}
	r.Device = uint64(ss.Dev)
	r.Inode = ss.Ino
	if retry {
		if done, err := checkpoint.RecoveryRead(filepath.Join(target, checkpoint.RecoveryComplete)); err == nil {
			if done != r {
				return errors.New("completion receipt mismatch")
			}
			return checkpoint.RejectRecoveryRun(target)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	receipt, _ := json.Marshal(r)
	if !retry {
		if e = backupRootPublish(sr, checkpoint.RecoverySource, receipt); e != nil {
			return e
		}
	}
	stage := func(s string) error {
		if hook != nil {
			return hook(s)
		}
		return nil
	}
	if e = stage("retired"); e != nil {
		return e
	}
	put := func(path string, b []byte) error {
		dir, err := checkpoint.BackupOpenRoot(filepath.Join(target, filepath.Dir(path)))
		if err != nil {
			return err
		}
		defer dir.Close()
		if e := backupRootPublish(dir, filepath.Base(path), b); e != nil {
			existing, readErr := checkpoint.BackupReadFile(filepath.Join(target, path), checkpoint.BackupFileLimit)
			if readErr != nil || !bytes.Equal(existing, b) {
				return fmt.Errorf("recovery target conflict %s: %w", path, e)
			}
		}
		return nil
	}
	if e = put(checkpoint.RecoveryTarget, receipt); e != nil {
		return e
	}
	if e = put("RECOVERY_ORIGINAL_STATE.json", original); e != nil {
		return e
	}
	for _, entry := range m.Files {
		if e = tr.MkdirAll(filepath.Dir(entry.Path), 0700); e != nil {
			return e
		}
		b := entry.Data
		if entry.Path == "state.json" {
			var migrated model.State
			if e = json.Unmarshal(b, &migrated); e != nil {
				return e
			}
			changed := version != migrated.Version
			migrated.Version = version
			if version != m.StateVersion {
				if migrated.Plan.APIVersion != model.GovernedPlanVersion {
					return errors.New("v4 migration requires unchanged governed plan semantics")
				}
				migrated.CheckpointMigration = &model.CheckpointMigration{Schema: "irrigator.checkpoint-migration/v1", FromVersion: 4, ToVersion: 5, OriginalStateSHA256: checkpoint.RecoveryDigest(original)}
			}
			for _, row := range migrated.Tasks {
				if row.Status == "running" {
					row.Status = "uncertain"
					row.Error = "source retired during recovery; reconcile original external outcome before explicit retry"
					if e = managed.ManagedHoldUncertain(&migrated, row); e != nil {
						return e
					}
					changed = true
				}
			}
			if changed {
				b, e = json.Marshal(migrated)
				if e != nil {
					return e
				}
			}
		}
		if e = put(entry.Path, b); e != nil {
			return e
		}
	}
	// Persist directory links bottom-up before completion can grant authority.
	for _, entry := range m.Files {
		for d := filepath.Dir(entry.Path); ; d = filepath.Dir(d) {
			f, err := tr.Open(d)
			if err != nil {
				return err
			}
			err = f.Sync()
			f.Close()
			if err != nil {
				return err
			}
			if d == "." {
				break
			}
		}
	}
	if e = stage("copied"); e != nil {
		return e
	}
	if campaign != nil {
		l, e := campaign.Read()
		if e != nil {
			return e
		}
		g, ok := l.Grants[state.Plan.Campaign.GrantID]
		if !ok || g.PlanHash != state.PlanHash || (g.RunDirectory != source && g.RunDirectory != target) {
			return errors.New("campaign grant no longer belongs to source")
		}
		if g.RunDirectory == source {
			data, _ := json.Marshal(l)
			if m.Campaign == nil || !bytes.Equal(data, m.Campaign.Ledger.Data) {
				return errors.New("campaign changed since backup")
			}
			g.RunDirectory = target
			l.Grants[g.ID] = g
			if e = campaign.Write(l); e != nil {
				return e
			}
		}
	}
	if e = stage("campaign"); e != nil {
		return e
	}
	if e = put(checkpoint.RecoveryComplete, receipt); e != nil {
		return e
	}
	return stage("complete")
}
