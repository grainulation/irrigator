package model

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"unicode/utf8"

	"github.com/grainulation/irrigator/internal/jsonio"
)

// ArtifactContract is Irrigator's bounded JSON contract, not JSON Schema.
// Every object property is required; undeclared properties and null are forbidden.
type ArtifactContract struct {
	Type       string                      `json:"type"`
	MaxBytes   int                         `json:"max_bytes"`
	Properties map[string]ArtifactContract `json:"properties,omitempty"`
	Items      *ArtifactContract           `json:"items,omitempty"`
	MaxItems   int                         `json:"max_items,omitempty"`
}

// Decode contracts strictly even when embedded in plans/checkpoints with optional
// fields: encoding/json otherwise treats null as an omitted zero value.
func (c *ArtifactContract) UnmarshalJSON(raw []byte) error {
	if len(raw) > 1048576 || !utf8.Valid(raw) {
		return errors.New("artifact contract byte/encoding bound exceeded")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	value, err := jsonio.ParseStrictJSON(d, 0)
	if err != nil {
		return err
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("trailing artifact contract JSON")
	}
	var nonnull func(any) error
	nonnull = func(v any) error {
		if v == nil {
			return errors.New("null artifact contract field")
		}
		switch v := v.(type) {
		case map[string]any:
			for _, x := range v {
				if err := nonnull(x); err != nil {
					return err
				}
			}
		case []any:
			for _, x := range v {
				if err := nonnull(x); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err = nonnull(value); err != nil {
		return err
	}
	type plain ArtifactContract
	if err = ExactTurnJSONFields(value, reflect.TypeOf(plain{})); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var decoded plain
	if err = decoder.Decode(&decoded); err != nil {
		return err
	}
	if err = ValidateArtifactContract(ArtifactContract(decoded)); err != nil {
		return err
	}
	*c = ArtifactContract(decoded)
	return nil
}

func ValidateArtifactContract(c ArtifactContract) error {
	count := 0
	var visit func(ArtifactContract, int) error
	visit = func(c ArtifactContract, depth int) error {
		count++
		if depth > 8 || count > 256 || c.MaxBytes < 1 || c.MaxBytes > 1048576 {
			return errors.New("artifact contract exceeds depth/node/byte bounds")
		}
		switch c.Type {
		case "string", "integer", "number", "boolean":
			if c.Properties != nil || c.Items != nil || c.MaxItems != 0 {
				return errors.New("primitive artifact contract has container constraints")
			}
		case "object":
			if c.Items != nil || c.MaxItems != 0 || len(c.Properties) > 128 {
				return errors.New("invalid object contract")
			}
			for k, v := range c.Properties {
				if k == "" || len(k) > 128 || !utf8.ValidString(k) {
					return errors.New("invalid artifact property")
				}
				if err := visit(v, depth+1); err != nil {
					return err
				}
			}
		case "array":
			if c.Properties != nil || c.Items == nil || c.MaxItems < 1 || c.MaxItems > 1024 {
				return errors.New("array requires bounded item contract")
			}
			return visit(*c.Items, depth+1)
		default:
			return errors.New("unsupported artifact type")
		}
		return nil
	}
	return visit(c, 0)
}
