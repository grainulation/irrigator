package checkpoint

import (
	"errors"

	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/model"
)

// validateManagedBinding checks the subset this dispatcher actually implements.
// Offline catalog capabilities are declarations, not proof of live support.
func ValidateManagedBinding(b model.ExecutionBinding) error {
	digest := b.Digest
	b.Digest = ""
	if digest == "" || digest != launch.ExecutionDigest(b) || b.Schema != launch.ExecutionBindingSchema {
		return errors.New("managed binding digest mismatch")
	}
	c := model.ExecutionCatalog{Schema: launch.ExecutionCatalogSchema, Adapters: map[string]model.ExecutionAdapter{b.AdapterName: b.Adapter}, Instances: map[string]model.ExecutionInstance{b.InstanceName: b.Instance}, Profiles: map[string]model.ExecutionProfile{b.ProfileName: b.Profile}, DefaultProfile: b.ProfileName}
	if err := launch.ValidateExecutionCatalog(c); err != nil {
		return err
	}
	if b.InstanceName != b.Profile.Instance || b.AdapterName != b.Instance.Adapter {
		return errors.New("managed binding references disagree")
	}
	amount, err := launch.ExecutionBudgetMicros(b.Profile.OperationBudgetUSD)
	if err != nil || amount != b.OperationBudgetMicros {
		return errors.New("managed binding budget mismatch")
	}
	if b.Adapter.Kind == "claude_cli" {
		ref := b.Instance.CredentialRefs[0]
		if ref.Kind != "host_session" || ref.Name != "claude" {
			return errors.New("managed Claude requires explicit host_session claude; env credential mapping unsupported")
		}
	}
	if b.Adapter.Kind == "codex_cli" {
		ref := b.Instance.CredentialRefs[0]
		if ref.Kind != "host_session" || ref.Name != "codex" {
			return errors.New("managed Codex requires explicit host_session codex")
		}
	}
	if b.Adapter.Kind == "agy_cli" {
		ref := b.Instance.CredentialRefs[0]
		if ref.Kind != "host_session" || ref.Name != "agy" {
			return errors.New("managed Antigravity requires explicit host_session agy")
		}
	}
	return nil
}
