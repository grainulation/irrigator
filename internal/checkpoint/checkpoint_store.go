package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"time"

	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/phasetrace"
	"github.com/grainulation/irrigator/internal/store"
)

// checkpointStore is a run-scoped transaction capability. Read returns an owned,
// detached snapshot. Update serializes admission, invokes its callback at most
// once on a private snapshot, and durably publishes only a successful callback.
// A callback error aborts publication. A publication error can be ambiguous:
// rename may have succeeded before directory fsync failed. Callers must reconcile
// durable operation identity instead of replaying the callback or provider call.
// Implementations must not implicitly retry callbacks on conflicts. This boundary
// does not grant execution ownership or replace campaign/billing authority.
// Alternate implementations must enforce equivalent checkpoint validation and
// authority gates before invoking reducers; storage conformance alone does not
// demonstrate authorization or distributed execution safety.
type CheckpointStore interface {
	Read(context.Context) (*model.State, error)
	Update(context.Context, CheckpointUpdateOptions, func(*model.State) error) error
}

type CheckpointUpdateOptions struct {
	// Nonblocking applies to every coordination lock acquired by the transaction;
	// it does not promise a deadline on arbitrary filesystem I/O. Blocking
	// flock cannot be interrupted by context cancellation; cancellation is
	// checked before and after lock acquisition and before callback/publication.
	Nonblocking bool
}

type FileCheckpointStore struct{ Root string }

// ErrUnchanged aborts a reducer successfully without publishing, migrating or
// recording an event. Reducers must decide this before mutating their snapshot.
var ErrUnchanged = errors.New("checkpoint unchanged")

func (f FileCheckpointStore) Read(ctx context.Context) (*model.State, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s, err := readFileCheckpoint(f.Root)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s, nil
}

func (f FileCheckpointStore) Update(ctx context.Context, options CheckpointUpdateOptions, fn func(*model.State) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := RejectQuarantinedRun(f.Root); err != nil {
		return err
	}
	phasetrace.Emit("ckpt", "upd_enter", "")
	prewarmJournal(f.Root)
	phasetrace.Emit("ckpt", "upd_prewarmed", "")
	waitStart := time.Now()
	lock, err := FileLock(filepath.Join(f.Root, "state.lock"), options.Nonblocking)
	if err != nil {
		return err
	}
	acquired := time.Now()
	phasetrace.Emit("ckpt", "upd_locked", "")
	checkpointLockWaitNanos.Add(uint64(acquired.Sub(waitStart)))
	defer func() {
		checkpointLockHeldNanos.Add(uint64(time.Since(acquired)))
		Unlock(lock)
		phasetrace.Emit("ckpt", "upd_unlocked", "")
	}()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := RejectQuarantinedRun(f.Root); err != nil {
		return err
	}
	s, raw, base, err := readCheckpointForUpdate(f.Root)
	if err != nil {
		return err
	}
	phasetrace.Emit("ckpt", "upd_read", "")
	if err := ValidateCampaignAuthority(f.Root, s.Plan, options.Nonblocking); err != nil {
		return err
	}
	if err := ValidateBillingAuthority(s); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// A journal read hands back its cache entry as an immutable base equal to s,
	// so the before-image costs nothing; otherwise take a private copy.
	before := base
	if before == nil {
		before = cloneCheckpoint(s)
	}
	planGuard, err := guardSharedPlan(f.Root, base)
	if err != nil {
		return err
	}
	if err := fn(s); err != nil {
		if gerr := planGuard(); gerr != nil {
			return gerr
		}
		if errors.Is(err, ErrUnchanged) {
			checkpointNoops.Add(1)
			return nil
		}
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := planGuard(); err != nil {
		return err
	}
	phasetrace.Emit("ckpt", "upd_callback_done", "")
	// The journal diff reports an unchanged state as no patches and counts it as
	// a no-op itself, so it needs no separate whole-state comparison first.
	if isJournalCheckpoint(raw) {
		return publishJournalUpdate(f.Root, raw, before, s)
	}
	if s.SchemaVersion >= model.CurrentSchemaVersion && reflect.DeepEqual(before, s) {
		checkpointNoops.Add(1)
		return nil
	}
	if s.SchemaVersion < model.CurrentSchemaVersion {
		if err := store.EnsureBackup(filepath.Join(f.Root, "state.json")); err != nil {
			return fmt.Errorf("migration backup failed: %w", err)
		}
		s.SchemaVersion = model.CurrentSchemaVersion
	}
	return publishPlainCheckpoint(f.Root, s)
}
