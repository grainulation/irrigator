package provider

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/commandflags"
	"github.com/grainulation/irrigator/internal/jsonio"
	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/localfs"
	"github.com/grainulation/irrigator/internal/model"
)

func ProviderMigrationEligibility(s *model.State, profile string) ([]string, error) {
	if !checkpoint.ManagedRun(s) || s.Execution == nil {
		return nil, errors.New("provider migration requires a managed run")
	}
	if s.Control != "pause" {
		return nil, errors.New("pause the run before preparing provider migration")
	}
	if s.Plan.Campaign != nil {
		return nil, errors.New("campaign-bound runs cannot migrate provider executables")
	}
	b, ok := s.Execution.Bindings[profile]
	if !ok || (b.Adapter.Kind != "claude_cli" && b.Adapter.Kind != "codex_cli") {
		return nil, errors.New("select an admitted Claude or Codex CLI profile")
	}
	if launch.ManagedHistoryEnabled(b.Profile) {
		return nil, errors.New("explicit history profiles cannot migrate provider executables")
	}
	if len(s.Execution.Migrations) >= checkpoint.MaxProviderMigrations {
		return nil, errors.New("provider migration limit reached")
	}
	ids := []string{}
	for id, row := range s.Tasks {
		if row.Status == "running" || row.Status == "uncertain" || row.Status == "produced" {
			return nil, fmt.Errorf("task %s must be stopped and reconciled before migration", id)
		}
		if row.Status == "pending" && model.ManagedProfile(s.Plan, row.Spec) == profile {
			if _, exists := s.Execution.Reservations[row.OperationID]; exists {
				return nil, fmt.Errorf("task %s still has a reserved operation; reconcile before migration", id)
			}
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	if len(ids) == 0 {
		return nil, errors.New("no pending tasks use this profile; explicitly retry settled failed work before migration")
	}
	return ids, nil
}
func hashMigrationExecutable(path string) (string, string, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return "", "", err
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return "", "", err
	}
	f, err := launch.OpenContextFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", "", err
	}
	if !st.Mode().IsRegular() || st.Mode().Perm()&0111 == 0 || st.Size() > launch.ExecutionExecutableLimit {
		return "", "", errors.New("replacement must be a bounded executable regular file")
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, launch.ExecutionExecutableLimit+1))
	if err != nil {
		return "", "", err
	}
	if n > launch.ExecutionExecutableLimit {
		return "", "", errors.New("replacement executable too large")
	}
	return path, hex.EncodeToString(h.Sum(nil)), nil
}
func PrepareProviderMigration(root, profile, executable string) (model.ProviderMigrationProposal, error) {
	var p model.ProviderMigrationProposal
	if err := checkpoint.RejectQuarantinedRun(root); err != nil {
		return p, err
	}
	s, err := checkpoint.ReadState(root)
	if err != nil {
		return p, err
	}
	ids, err := ProviderMigrationEligibility(s, profile)
	if err != nil {
		return p, err
	}
	path, hash, err := hashMigrationExecutable(executable)
	if err != nil {
		return p, err
	}
	b, err := checkpoint.MigrationBinding(s, profile, path, hash)
	if err != nil {
		return p, err
	}
	if b.Digest == model.ManagedCurrentBinding(s, profile).Digest {
		return p, errors.New("replacement is already the active binding")
	}
	// Verify again through the normal dispatch binding gate; never launch it.
	if err := verifyMigrationExecutable(b); err != nil {
		return p, err
	}
	p = model.ProviderMigrationProposal{Schema: checkpoint.ProviderMigrationSchema, RunID: s.RunID, PlanHash: s.PlanHash, BaseStateDigest: launch.ExecutionDigest(s), Sequence: len(s.Execution.Migrations) + 1, Profile: profile, FromBinding: model.ManagedCurrentBinding(s, profile).Digest, Replacement: b, PendingTasks: ids}
	p.Digest = checkpoint.ProviderMigrationDigest(p)
	return p, nil
}
func ApplyProviderMigration(root string, p model.ProviderMigrationProposal, approval string, confirmed bool) error {
	if !confirmed {
		return errors.New("provider migration requires --confirm-stopped after verifying all provider work has stopped")
	}
	if approval == "" || approval != p.Digest || p.Digest != checkpoint.ProviderMigrationDigest(p) {
		return errors.New("provider migration approval digest mismatch")
	}
	if err := checkpoint.RejectQuarantinedRun(root); err != nil {
		return err
	}
	lock, err := checkpoint.FileLock(filepath.Join(root, "controller.lock"), true)
	if err != nil {
		return errors.New("stop the controller before provider migration")
	}
	defer checkpoint.Unlock(lock)
	s, err := checkpoint.ReadState(root)
	if err != nil {
		return err
	}
	attempts := ProviderMigrationAttempts(s)
	ids := make([]string, 0, len(attempts))
	for id := range attempts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	leases := []*os.File{}
	defer func() {
		for _, l := range leases {
			checkpoint.Unlock(l)
		}
	}()
	for _, id := range ids {
		if s.Tasks[id] == nil { // A garbage-collected cell has no process evidence left.
			if _, e := os.Lstat(filepath.Join(root, "cells", id)); os.IsNotExist(e) {
				continue
			}
		}
		l, e := checkpoint.FileLock(filepath.Join(root, "cells", id, "lease"), true)
		if e != nil {
			return fmt.Errorf("task %s has an active worker", id)
		}
		leases = append(leases, l)
	}
	return checkpoint.Update(root, func(s *model.State) error {
		if launch.ExecutionDigest(s) != p.BaseStateDigest {
			return errors.New("run changed after migration preparation; prepare a fresh proposal")
		}
		pending, err := ProviderMigrationEligibility(s, p.Profile)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(pending, p.PendingTasks) {
			return errors.New("migration pending set changed")
		}
		for _, id := range ids {
			for attempt := 1; attempt <= attempts[id]; attempt++ {
				paths, e := launch.ProcessMarkerPaths(filepath.Join(root, "cells", id), attempt)
				if e != nil {
					return e
				}
				for _, path := range paths {
					raw, e := readMigrationProcessEvidence(path)
					if os.IsNotExist(e) {
						continue
					}
					var marker model.ProcessMarker
					if e != nil || json.Unmarshal(raw, &marker) != nil || marker.Status != "stopped" || marker.CleanupError != "" {
						return fmt.Errorf("task %s attempt %d cleanup evidence is unresolved", id, attempt)
					}
				}
			}
		}
		if err := verifyMigrationExecutable(p.Replacement); err != nil {
			return err
		}
		s.Execution.Migrations = append(s.Execution.Migrations, model.ProviderMigrationRecord{Proposal: p, ApprovedAt: time.Now().UTC(), ConfirmedStopped: true})
		if err := checkpoint.ValidateProviderMigrations(s); err != nil {
			return err
		}
		for _, id := range pending {
			s.Tasks[id].BindingDigest = p.Replacement.Digest
		}
		s.Event("provider_migrated", "", map[string]any{"profile": p.Profile, "migration_digest": p.Digest, "from_binding": p.FromBinding, "to_binding": p.Replacement.Digest, "pending_tasks": len(pending)})
		return checkpoint.ValidateManagedState(s)
	})
}
func ProviderMigrationCLI(args []string) (int, error) {
	f := flag.NewFlagSet("profiles "+args[0], flag.ContinueOnError)
	root := f.String("run", "", "paused managed run")
	profile := f.String("profile", "", "admitted profile to migrate")
	executable := f.String("executable", "", "replacement executable; resolved and hashed without launching")
	out := f.String("out", "", "new proposal file")
	proposal := f.String("proposal", "", "reviewed proposal file")
	approve := f.String("approve", "", "exact proposal digest")
	stopped := f.Bool("confirm-stopped", false, "attest all provider work has stopped; active markers still reject")
	if err := commandflags.ParseCommandFlags(f, args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0, nil
		}
		return 2, err
	}
	if *root == "" || f.NArg() != 0 {
		return 2, errors.New("migration requires --run and no positional arguments")
	}
	if args[0] == "migrate-prepare" {
		if *profile == "" || *executable == "" || *out == "" || *proposal != "" || *approve != "" || *stopped {
			return 2, errors.New("migrate-prepare requires --run --profile --executable --out")
		}
		p, err := PrepareProviderMigration(*root, *profile, *executable)
		if err != nil {
			return 1, err
		}
		data, err := json.MarshalIndent(p, "", "  ")
		if err != nil {
			return 1, err
		}
		if err = localfs.WriteNew(*out, append(data, '\n')); err != nil {
			return 1, err
		}
		return 0, jsonio.PrintJSON(map[string]any{"proposal": *out, "approval_digest": p.Digest, "executable_verified": true, "compatibility_probed": false, "dispatched": false})
	}
	if *proposal == "" || *approve == "" || *profile != "" || *executable != "" || *out != "" {
		return 2, errors.New("migrate-apply requires --run --proposal --approve --confirm-stopped")
	}
	data, err := checkpoint.BackupReadFile(*proposal, 1<<20)
	if err != nil {
		return 1, err
	}
	var p model.ProviderMigrationProposal
	if err = DecodeMigrationProposal(data, &p); err != nil {
		return 1, err
	}
	if err = ApplyProviderMigration(*root, p, *approve, *stopped); err != nil {
		return 1, err
	}
	return 0, jsonio.PrintJSON(map[string]any{"applied": true, "migration_digest": p.Digest, "run_remains_paused": true, "dispatched": false})
}

func verifyMigrationExecutable(b model.ExecutionBinding) error {
	if err := checkpoint.ValidateManagedBinding(b); err != nil {
		return err
	}
	if len(b.Adapter.Command) != 1 {
		return errors.New("replacement must have one executable")
	}
	path, hash, err := hashMigrationExecutable(b.Adapter.Command[0])
	if err != nil {
		return err
	}
	if path != b.Adapter.Command[0] || hash != b.Adapter.ExecutableSHA256 {
		return errors.New("replacement executable changed after preparation")
	}
	return nil
}
func DecodeMigrationProposal(data []byte, p *model.ProviderMigrationProposal) error {
	if !utf8.Valid(data) {
		return errors.New("invalid proposal UTF-8")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	v, err := jsonio.ParseStrictJSON(d, 0)
	if err != nil {
		return err
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("trailing proposal data")
	}
	if err = model.ExactTurnJSONFields(v, reflect.TypeOf(*p)); err != nil {
		return err
	}
	return json.Unmarshal(data, p)
}

func ProviderMigrationAttempts(s *model.State) map[string]int {
	attempts := map[string]int{}
	for id, row := range s.Tasks {
		attempts[id] = row.Attempt
	}
	for _, row := range s.ArchivedTasks {
		if row.Attempt > attempts[row.Spec.ID] {
			attempts[row.Spec.ID] = row.Attempt
		}
	}
	return attempts
}

func readMigrationProcessEvidence(path string) ([]byte, error) {
	path, err := checkpoint.BackupAbsolutePath(path)
	if err != nil {
		return nil, err
	}
	f, err := launch.OpenContextFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() || st.Size() > 65536 {
		return nil, errors.New("process marker must be a bounded regular file")
	}
	raw, err := io.ReadAll(io.LimitReader(f, 65537))
	if err != nil {
		return nil, err
	}
	if len(raw) > 65536 {
		return nil, errors.New("process marker too large")
	}
	object, ok := StrictCostObject(raw, "status", "supervisor_pid", "confirmed_by", "previous_status", "cleanup_error")
	if !ok || object["status"] != "stopped" {
		return nil, errors.New("process marker is ambiguous or not stopped")
	}
	return raw, nil
}
