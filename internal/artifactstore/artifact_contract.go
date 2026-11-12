package artifactstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/grainulation/irrigator/internal/jsonio"
	"github.com/grainulation/irrigator/internal/model"
)

func ValidateArtifactContracts(t model.Task) error {
	if len(t.ArtifactContracts) == 0 {
		return nil
	}
	if len(t.ArtifactContracts) != len(t.Requires) {
		return errors.New("artifact contracts must cover every required output")
	}
	for _, key := range t.Requires {
		c, ok := t.ArtifactContracts[key]
		if !ok {
			return errors.New("missing artifact contract")
		}
		if err := model.ValidateArtifactContract(c); err != nil {
			return fmt.Errorf("artifact %s: %w", key, err)
		}
	}
	return nil
}

func ArtifactValue(c model.ArtifactContract, raw string) (string, error) {
	if err := model.ValidateArtifactContract(c); err != nil {
		return "", err
	}
	if len(raw) > c.MaxBytes || !utf8.ValidString(raw) {
		return "", errors.New("artifact byte/encoding bound exceeded")
	}
	d := json.NewDecoder(strings.NewReader(raw))
	d.UseNumber()
	v, err := jsonio.ParseStrictJSON(d, 0)
	if err != nil {
		return "", err
	}
	if d.Decode(new(any)) != io.EOF {
		return "", errors.New("trailing artifact JSON")
	}
	var check func(model.ArtifactContract, any) error
	check = func(c model.ArtifactContract, v any) error {
		b, err := json.Marshal(v)
		if err != nil || len(b) > c.MaxBytes {
			return errors.New("nested artifact byte bound exceeded")
		}
		good := false
		switch c.Type {
		case "string":
			_, good = v.(string)
		case "boolean":
			_, good = v.(bool)
		case "number", "integer":
			n, ok := v.(json.Number)
			if ok {
				f, e := n.Float64()
				good = e == nil && !math.IsInf(f, 0) && !math.IsNaN(f)
				if good && c.Type == "integer" {
					text := string(n)
					if i := strings.IndexAny(text, "eE"); i >= 0 {
						exp, e := strconv.Atoi(text[i+1:])
						if e != nil || exp < -308 || exp > 308 {
							return errors.New("integer exponent exceeds bound")
						}
					}
					r, ok := new(big.Rat).SetString(text)
					good = ok && r.IsInt()
				}
			}
		case "object":
			o, ok := v.(map[string]any)
			good = ok && len(o) == len(c.Properties)
			if good {
				for k, sub := range c.Properties {
					x, ok := o[k]
					if !ok {
						return errors.New("missing artifact property")
					}
					if err := check(sub, x); err != nil {
						return fmt.Errorf("property %s: %w", k, err)
					}
				}
			}
		case "array":
			a, ok := v.([]any)
			good = ok && len(a) <= c.MaxItems
			if good {
				for _, x := range a {
					if err := check(*c.Items, x); err != nil {
						return err
					}
				}
			}
		}
		if !good {
			return fmt.Errorf("artifact does not satisfy %s contract", c.Type)
		}
		return nil
	}
	if err = check(c, v); err != nil {
		return "", err
	}
	b, err := json.Marshal(v)
	return string(b), err
}

func AcceptArtifactContracts(t model.Task, result map[string]string) error {
	if err := ValidateArtifactContracts(t); err != nil {
		return err
	}
	for key, c := range t.ArtifactContracts {
		canonical, err := ArtifactValue(c, result[key])
		if err != nil {
			return fmt.Errorf("artifact %s: %w", key, err)
		}
		if canonical != result[key] {
			return fmt.Errorf("artifact %s must use canonical compact JSON with sorted object keys", key)
		}
	}
	return nil
}

// A producer may narrow byte/item bounds, but may not widen a consumer's contract.
func ArtifactAssignable(source, target model.ArtifactContract) bool {
	if source.Type != target.Type && !(source.Type == "integer" && target.Type == "number") {
		return false
	}
	if source.MaxBytes > target.MaxBytes {
		return false
	}
	if source.Type == "object" {
		if len(source.Properties) != len(target.Properties) {
			return false
		}
		for k, s := range source.Properties {
			t, ok := target.Properties[k]
			if !ok || !ArtifactAssignable(s, t) {
				return false
			}
		}
	}
	if source.Type == "array" {
		return source.MaxItems <= target.MaxItems && source.Items != nil && target.Items != nil && ArtifactAssignable(*source.Items, *target.Items)
	}
	return true
}
