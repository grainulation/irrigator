package artifactstore

import (
	"errors"
	"fmt"
	"strings"

	"github.com/grainulation/irrigator/internal/model"
)

func Accept(t model.Task, result map[string]string) error {
	if err := AcceptArtifactContracts(t, result); err != nil {
		return err
	}
	if len(result) != len(t.Requires) {
		return errors.New("output fields do not match contract")
	}
	for _, key := range t.Requires {
		value, present := result[key]
		// A declared turn request has an explicit empty stop value. This is
		// control output, not a missing answer; all other fields remain nonempty.
		if present && key == TurnRequestField && t.Turn != nil && strings.TrimSpace(value) == "" {
			continue
		}
		if !present || strings.TrimSpace(value) == "" {
			return fmt.Errorf("missing nonempty field: %s", key)
		}
	}
	for _, key := range t.FileFields {
		if _, err := DecodeFileMap([]byte(result[key])); err != nil {
			return fmt.Errorf("file field %q: %w", key, err)
		}
	}
	for key, expected := range t.Equals {
		if result[key] != expected {
			return fmt.Errorf("acceptance mismatch: %s", key)
		}
	}
	return nil
}
