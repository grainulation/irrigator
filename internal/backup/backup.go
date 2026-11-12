package backup

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/commandflags"
	"github.com/grainulation/irrigator/internal/jsonio"
	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/model"
)

const backupLimit = 64 << 20

type backupEntry struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	Data   []byte `json:"data"`
}
type backupManifest struct {
	Schema        string                  `json:"schema"`
	Created       string                  `json:"created"`
	RunID         string                  `json:"run_id"`
	StateVersion  int                     `json:"state_version"`
	PlanHash      string                  `json:"plan_hash"`
	Compatibility string                  `json:"compatibility"`
	Files         []backupEntry           `json:"files"`
	Campaign      *backupCampaignEvidence `json:"campaign,omitempty"`
}

func BackupCLI(args []string) (int, error) {
	if len(args) == 0 {
		return 2, errors.New("usage: backup create --run RUN --out FILE | verify --in FILE | restore --in FILE --out NEW_DIRECTORY")
	}
	f := flag.NewFlagSet("backup "+args[0], flag.ContinueOnError)
	src := f.String("run", "", "source run")
	in := f.String("in", "", "bundle")
	out := f.String("out", "", "new destination")
	if err := commandflags.ParseCommandFlags(f, args[1:]); err != nil {
		return 2, err
	}
	if f.NArg() != 0 {
		return 2, errors.New("unexpected backup arguments")
	}
	var err error
	switch args[0] {
	case "create":
		if *src == "" || *out == "" || *in != "" {
			return 2, errors.New("create requires --run and --out")
		}
		err = BackupCreate(*src, *out)
	case "verify":
		if *in == "" || *src != "" || *out != "" {
			return 2, errors.New("verify requires --in")
		}
		_, err = BackupRead(*in)
	case "restore":
		if *in == "" || *out == "" || *src != "" {
			return 2, errors.New("restore requires --in and --out")
		}
		err = BackupRestore(*in, *out)
	default:
		return 2, errors.New("unknown backup operation")
	}
	if err != nil {
		return 1, err
	}
	message := "Backup evidence verified; this does not grant permission to execute a restored run."
	if args[0] == "create" {
		message = "Backup created and verified. The source run is unchanged; this bundle carries evidence, not execution authority."
	} else if args[0] == "restore" {
		message = "Restored into quarantine for inspection. Do not remove quarantine markers or resume the copied identity; use the documented recovery transfer from the original run when execution is required."
	}
	return 0, jsonio.PrintJSON(map[string]any{"operation": args[0], "verified": true, "execution_authorized": false, "message": message, "guide": "docs/backup-restore.md"})
}

func BackupInventory(root string) ([]backupEntry, error) {
	var entries []backupEntry
	var total int64
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			return errors.New("backup rejects symlinks")
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if len(entries) >= 4096 {
			return errors.New("backup file count exceeded")
		}
		data, err := checkpoint.BackupReadFile(path, checkpoint.BackupFileLimit)
		if err != nil {
			return fmt.Errorf("backup %s: %w", rel, err)
		}
		total += int64(len(data))
		if total > backupLimit/2 {
			return errors.New("backup payload exceeds limit")
		}
		h := sha256.Sum256(data)
		entries = append(entries, backupEntry{filepath.ToSlash(rel), int64(len(data)), hex.EncodeToString(h[:]), data})
		return nil
	})
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return entries, err
}
func BackupCreate(root, out string) error {
	if err := checkpoint.RejectQuarantinedRun(root); err != nil {
		return err
	}
	if err := checkpoint.RejectQuarantinedRun(out); err != nil {
		return err
	}
	root, err := checkpoint.BackupAbsolutePath(root)
	if err != nil {
		return err
	}
	out, err = checkpoint.BackupAbsolutePath(out)
	if err != nil {
		return err
	}
	if out == root || strings.HasPrefix(out, root+string(os.PathSeparator)) {
		return errors.New("backup output must be outside source")
	}
	// Validate every file and directory before opening any writable lock.
	if _, err = BackupInventory(root); err != nil {
		return err
	}
	var locks []*os.File
	defer func() {
		for i := len(locks) - 1; i >= 0; i-- {
			checkpoint.Unlock(locks[i])
		}
	}()
	take := func(rel string) error {
		f, e := launch.OpenContextFile(filepath.Join(root, rel), os.O_CREATE|os.O_RDWR, 0600)
		if e != nil {
			return e
		}
		st, e := f.Stat()
		if e != nil || !st.Mode().IsRegular() || st.Sys().(*syscall.Stat_t).Nlink != 1 {
			f.Close()
			return errors.New("unsafe backup lock")
		}
		if e = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
			f.Close()
			return errors.New("backup source is active or locked")
		}
		locks = append(locks, f)
		return nil
	}
	for _, p := range []string{"delivery.lock", "controller.lock", "state.lock"} {
		if err = take(p); err != nil {
			return err
		}
	}
	data, err := checkpoint.BackupReadFile(filepath.Join(root, "state.json"), checkpoint.BackupFileLimit)
	if err != nil {
		return err
	}
	var s model.State
	if err = json.Unmarshal(data, &s); err != nil {
		return err
	}
	if s.Version < 1 || s.Version > 5 || s.RunID == "" {
		return errors.New("unsupported backup checkpoint")
	}
	for _, t := range s.AllTasks() {
		if !RegistryRunID.MatchString(t.ID) {
			return errors.New("unsafe task ID")
		}
		cell := filepath.Join(root, "cells", t.ID)
		if _, e := os.Lstat(cell); errors.Is(e, os.ErrNotExist) {
			continue
		}
		if err = take(filepath.Join("cells", t.ID, "lease")); err != nil {
			return err
		}
	}
	campaign, releaseCampaign, err := backupCaptureCampaign(root, &s)
	if err != nil {
		return err
	}
	defer releaseCampaign()
	before, err := BackupInventory(root)
	if err != nil {
		return err
	}
	after, err := BackupInventory(root)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(before, after) {
		return errors.New("backup source changed during snapshot")
	}
	m := backupManifest{Schema: "irrigator.backup/v1", Created: time.Now().UTC().Format(time.RFC3339Nano), RunID: s.RunID, StateVersion: s.Version, PlanHash: s.PlanHash, Compatibility: backupCompatibilityV1, Files: before, Campaign: campaign}
	if s.Version == 4 || campaign != nil {
		m.Schema = "irrigator.backup/v2"
		m.Compatibility = backupCompatibilityV2
	}
	if s.Version == 5 {
		m.Schema = "irrigator.backup/v3"
		m.Compatibility = model.BackupCompatibilityV3
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if len(b) > backupLimit {
		return errors.New("backup exceeds encoded limit")
	}
	return backupPublish(out, b)
}
func BackupRead(path string) (backupManifest, error) {
	var m backupManifest
	b, err := checkpoint.BackupReadFile(path, backupLimit)
	if err != nil {
		return m, err
	}
	strict := json.NewDecoder(bytes.NewReader(b))
	if _, err = jsonio.ParseStrictJSON(strict, 0); err != nil {
		return m, err
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err = d.Decode(&m); err != nil {
		return m, err
	}
	if d.Decode(new(any)) != io.EOF {
		return m, errors.New("trailing backup data")
	}
	if m.RunID == "" || m.StateVersion < 1 || m.StateVersion > 5 {
		return m, errors.New("unsupported backup manifest")
	}
	switch m.Schema {
	case "irrigator.backup/v1":
		if m.StateVersion > 3 || m.Campaign != nil || m.Compatibility != backupCompatibilityV1 {
			return m, errors.New("unsupported v1 backup manifest")
		}
	case "irrigator.backup/v2":
		if m.StateVersion > 4 || m.Compatibility != backupCompatibilityV2 {
			return m, errors.New("unsupported v2 backup compatibility")
		}
	case "irrigator.backup/v3":
		if m.StateVersion != 5 || m.Compatibility != model.BackupCompatibilityV3 {
			return m, errors.New("unsupported v3 backup compatibility")
		}
	default:
		return m, errors.New("unsupported backup manifest")
	}

	if _, err = time.Parse(time.RFC3339Nano, m.Created); err != nil {
		return m, err
	}
	if len(m.Files) == 0 || len(m.Files) > 4096 {
		return m, errors.New("invalid backup inventory")
	}
	last := ""
	var total int64
	var state *model.State
	for _, e := range m.Files {
		if e.Path <= last || !fs.ValidPath(e.Path) || strings.Contains(e.Path, "\\") || e.Path == checkpoint.BackupQuarantine || strings.HasPrefix(e.Path, checkpoint.BackupQuarantine+"/") {
			return m, errors.New("unsafe or duplicate inventory path")
		}
		last = e.Path
		if e.Size < 0 || e.Size > checkpoint.BackupFileLimit || e.Size != int64(len(e.Data)) {
			return m, errors.New("invalid inventory size")
		}
		total += e.Size
		if total > backupLimit/2 {
			return m, errors.New("inventory too large")
		}
		h := sha256.Sum256(e.Data)
		if hex.EncodeToString(h[:]) != e.SHA256 {
			return m, errors.New("backup digest mismatch")
		}
		if e.Path == "state.json" {
			state = &model.State{}
			if err = json.Unmarshal(e.Data, state); err != nil {
				return m, err
			}
		}
	}
	if state == nil || state.RunID != m.RunID || state.Version != m.StateVersion || state.PlanHash != m.PlanHash {
		return m, errors.New("checkpoint provenance mismatch")
	}
	if err = checkpoint.ValidateMailboxState(state); err != nil {
		return m, err
	}
	if err = validateBackupCampaign(state, m.Campaign); err != nil {
		return m, err
	}
	return m, nil
}
