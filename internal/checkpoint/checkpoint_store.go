package checkpoint

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/grainulation/irrigator/internal/jsonio"
	"github.com/grainulation/irrigator/internal/model"
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
	lock, err := FileLock(filepath.Join(f.Root, "state.lock"), options.Nonblocking)
	if err != nil {
		return err
	}
	defer Unlock(lock)
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := RejectQuarantinedRun(f.Root); err != nil {
		return err
	}
	s, err := f.Read(ctx)
	if err != nil {
		return err
	}
	if err := ValidateCampaignAuthority(f.Root, s.Plan, options.Nonblocking); err != nil {
		return err
	}
	if err := ValidateBillingAuthority(s); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := fn(s); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.SchemaVersion < model.CurrentSchemaVersion {
		if err := store.EnsureBackup(filepath.Join(f.Root, "state.json")); err != nil {
			return fmt.Errorf("migration backup failed: %w", err)
		}
		s.SchemaVersion = model.CurrentSchemaVersion
	}
	return jsonio.AtomicJSON(filepath.Join(f.Root, "state.json"), s)
}
