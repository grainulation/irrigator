package checkpoint

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"reflect"

	"github.com/grainulation/irrigator/internal/artifactstore"
	"github.com/grainulation/irrigator/internal/commandflags"
	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/model"
)

// planJSONSchema describes JSON structure, not runtime admission. In particular,
// omitempty controls Go encoding; it does not make an input field required.
func planJSONSchema() ([]byte, error) {
	b := &artifactstore.PlanSchemaBuilder{Defs: map[string]any{}}
	root := b.Schema(reflect.TypeOf(model.Plan{}))
	plan := b.Defs["Plan"].(map[string]any)
	props := plan["properties"].(map[string]any)
	props["api_version"].(map[string]any)["enum"] = []string{"", "swarm/v1", TurnPlanVersion, launch.ManagedPlanVersion, launch.UnknownCostPlanVersion, model.GovernedPlanVersion, launch.ArtifactPlanVersion}
	root["$schema"] = "https://json-schema.org/draft/2020-12/schema"
	root["title"] = "Irrigator plan"
	root["description"] = "Generated from the runtime Go Plan types for editor completion and structural checks. Run irrigator plan FILE for authoritative required-field, version, ownership, dependency, budget, artifact, and cross-field validation. x-go-omitempty describes encoding only. Interface and raw JSON fields are unconstrained."
	root["$defs"] = b.Defs
	raw, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}

func SchemaCLI(args []string) (int, error) {
	fs := flag.NewFlagSet("schema", flag.ContinueOnError)
	if err := commandflags.ParseCommandFlags(fs, args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0, nil
		}
		return 2, err
	}
	if fs.NArg() != 0 {
		return 2, fmt.Errorf("usage: irrigator schema")
	}
	raw, err := planJSONSchema()
	if err != nil {
		return 1, err
	}
	if _, err = os.Stdout.Write(raw); err != nil {
		return 1, err
	}
	return 0, nil
}
