package checkpoint

import (
	"encoding/json"
	"errors"
	"reflect"
	"regexp"
	"sort"

	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/model"
)

const ProviderMigrationSchema = "irrigator.provider-migration/v1"
const MaxProviderMigrations = 64

var migrationTaskID = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func ProviderMigrationDigest(p model.ProviderMigrationProposal) string {
	p.Digest = ""
	return launch.ExecutionDigest(p)
}

func ManagedBindingByDigest(s *model.State, profile, digest string) (model.ExecutionBinding, error) {
	b, ok := s.Execution.Bindings[profile]
	if ok && b.Digest == digest {
		return b, nil
	}
	for _, m := range s.Execution.Migrations {
		if m.Proposal.Profile == profile && m.Proposal.Replacement.Digest == digest {
			return m.Proposal.Replacement, nil
		}
	}
	return model.ExecutionBinding{}, errors.New("unknown historical provider binding")
}
func MigrationBinding(s *model.State, profile, path, hash string) (model.ExecutionBinding, error) {
	raw, _ := json.Marshal(s.Plan.Execution.Catalog)
	var c model.ExecutionCatalog
	if err := json.Unmarshal(raw, &c); err != nil {
		return model.ExecutionBinding{}, err
	}
	old, ok := s.Execution.Bindings[profile]
	if !ok || (old.Adapter.Kind != "claude_cli" && old.Adapter.Kind != "codex_cli") {
		return model.ExecutionBinding{}, errors.New("migration requires an admitted Claude or Codex CLI profile")
	}
	a := old.Adapter
	a.Command = []string{path}
	a.ExecutableSHA256 = hash
	c.Adapters[old.AdapterName] = a
	if err := launch.ValidateExecutionCatalog(c); err != nil {
		return model.ExecutionBinding{}, err
	}
	b := ManagedExpectedBinding(c, profile)
	return b, ValidateManagedBinding(b)
}
func ValidateProviderMigrations(s *model.State) error {
	if s.Execution == nil || len(s.Execution.Migrations) == 0 {
		return nil
	}
	if !ManagedRun(s) || s.Plan.Execution == nil || s.Plan.Campaign != nil || len(s.Execution.Migrations) > MaxProviderMigrations {
		return errors.New("unsupported provider migration state")
	}
	current := map[string]string{}
	for name, b := range s.Execution.Bindings {
		current[name] = b.Digest
	}
	for i, m := range s.Execution.Migrations {
		p := m.Proposal
		if p.Schema != ProviderMigrationSchema || p.RunID != s.RunID || p.PlanHash != s.PlanHash || p.Sequence != i+1 || len(p.BaseStateDigest) != 64 || p.Digest != ProviderMigrationDigest(p) || !m.ConfirmedStopped || m.ApprovedAt.IsZero() || p.FromBinding != current[p.Profile] {
			return errors.New("invalid provider migration provenance")
		}
		if launch.ManagedHistoryEnabled(p.Replacement.Profile) {
			return errors.New("provider migration does not support explicit history profiles")
		}
		if len(p.Replacement.Adapter.Command) != 1 {
			return errors.New("invalid replacement executable")
		}
		expected, err := MigrationBinding(s, p.Profile, p.Replacement.Adapter.Command[0], p.Replacement.Adapter.ExecutableSHA256)
		if err != nil || !reflect.DeepEqual(expected, p.Replacement) || p.FromBinding == p.Replacement.Digest {
			return errors.New("provider migration changed more than executable identity")
		}
		if len(p.PendingTasks) == 0 || !sort.StringsAreSorted(p.PendingTasks) {
			return errors.New("migration must name sorted pending tasks")
		}
		for j, id := range p.PendingTasks {
			if !migrationTaskID.MatchString(id) || (j > 0 && id == p.PendingTasks[j-1]) {
				return errors.New("invalid migration pending task identity")
			}
		}
		current[p.Profile] = p.Replacement.Digest
	}
	return nil
}
