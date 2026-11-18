package launch

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/grainulation/irrigator/internal/jsonio"
	"github.com/grainulation/irrigator/internal/model"
)

const ExecutionCatalogSchema = "irrigator.execution/v1"
const ExecutionBindingSchema = "irrigator.binding/v1"
const ExecutionCatalogLimit = 256 * 1024
const ExecutionExecutableLimit = 512 * 1024 * 1024

var ExecutionName = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_-]{0,63}$`)
var ExecutionModel = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.:/-]{0,127}$`)
var executionEnvName = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,127}$`)
var executionUSD = regexp.MustCompile(`^(0|[1-9][0-9]{0,6})(\.[0-9]{1,6})?$`)

func ExecutionBudgetMicros(value string) (int64, error) {
	if !executionUSD.MatchString(value) {
		return 0, errors.New("operation budget must be a nonnegative decimal USD string with at most six fractional digits")
	}
	parts := strings.Split(value, ".")
	whole, _ := strconv.ParseInt(parts[0], 10, 64)
	fraction := int64(0)
	if len(parts) == 2 {
		fraction, _ = strconv.ParseInt(parts[1]+strings.Repeat("0", 6-len(parts[1])), 10, 64)
	}
	amount := whole*1_000_000 + fraction
	if amount > 1_000_000_000_000 {
		return 0, errors.New("operation budget exceeds supported bound")
	}
	return amount, nil
}

// Reject null, aliases and omitted required fields recursively, before decoding
// into Go structs (whose standard decoder otherwise accepts case aliases/null).
func ExecutionJSONShape(value any, typ reflect.Type) error {
	if value == nil {
		return errors.New("catalog null values are not supported")
	}
	switch typ.Kind() {
	case reflect.Struct:
		object, ok := value.(map[string]any)
		if !ok {
			return errors.New("catalog expected object")
		}
		fields := map[string]reflect.StructField{}
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			tag := strings.Split(f.Tag.Get("json"), ",")
			fields[tag[0]] = f
			if len(tag) == 1 {
				if _, ok := object[tag[0]]; !ok {
					return fmt.Errorf("missing catalog field %s", tag[0])
				}
			}
		}
		for key, v := range object {
			f, ok := fields[key]
			if !ok {
				return errors.New("unknown or noncanonical catalog field")
			}
			if err := ExecutionJSONShape(v, f.Type); err != nil {
				return err
			}
		}
	case reflect.Map:
		object, ok := value.(map[string]any)
		if !ok {
			return errors.New("catalog expected map")
		}
		for _, v := range object {
			if err := ExecutionJSONShape(v, typ.Elem()); err != nil {
				return err
			}
		}
	case reflect.Slice:
		values, ok := value.([]any)
		if !ok {
			return errors.New("catalog expected array")
		}
		for _, v := range values {
			if err := ExecutionJSONShape(v, typ.Elem()); err != nil {
				return err
			}
		}
	}
	return nil
}

func parseExecutionCatalog(raw []byte) (model.ExecutionCatalog, error) {
	var catalog model.ExecutionCatalog
	if len(raw) == 0 || len(raw) > ExecutionCatalogLimit || !utf8.Valid(raw) {
		return catalog, errors.New("catalog exceeds input bound")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	value, err := jsonio.ParseStrictJSON(decoder, 0)
	if err != nil {
		return catalog, errors.New("invalid or duplicate catalog JSON")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return catalog, errors.New("trailing catalog JSON")
	}
	if err = ExecutionJSONShape(value, reflect.TypeOf(catalog)); err != nil {
		return catalog, err
	}
	if err = json.Unmarshal(raw, &catalog); err != nil {
		return catalog, errors.New("invalid catalog field type")
	}
	if err = ValidateExecutionCatalog(catalog); err != nil {
		return catalog, err
	}
	return catalog, nil
}

func ExecutionCapabilities(kind string) []string {
	caps := []string{"outcome_certainty", "structured_strings"}
	if kind == "claude_cli" {
		caps = append(caps, "explicit_history")
	}
	sort.Strings(caps)
	return caps
}
func ValidateExecutionCatalog(c model.ExecutionCatalog) error {
	if c.Schema != ExecutionCatalogSchema {
		return errors.New("unsupported execution catalog schema")
	}
	if len(c.Adapters) < 1 || len(c.Adapters) > 32 || len(c.Instances) < 1 || len(c.Instances) > 64 || len(c.Profiles) < 1 || len(c.Profiles) > 128 {
		return errors.New("catalog entry counts exceed bounds")
	}
	for name, a := range c.Adapters {
		if !ExecutionName.MatchString(name) || a.Protocol != model.ProtocolVersion {
			return errors.New("invalid adapter name or protocol")
		}
		if a.Kind != "ollama_http" && a.Endpoint != "" {
			return errors.New("endpoint only supported by Ollama")
		}
		switch a.Kind {
		case "ollama_http":
			if len(a.Command) != 0 || a.ExecutableSHA256 != "" {
				return errors.New("Ollama HTTP cannot declare executable")
			}
			if err := ValidateOllamaEndpoint(a.Endpoint); err != nil {
				return err
			}
		case "mock":
			if len(a.Command) != 0 || a.ExecutableSHA256 != "" {
				return errors.New("mock adapter cannot declare an executable")
			}
		case "claude_cli", "codex_cli", "agy_cli":
			if len(a.Command) != 1 || !filepath.IsAbs(a.Command[0]) || filepath.Clean(a.Command[0]) != a.Command[0] || !ContextText(a.Command[0], 4096) {
				return errors.New("CLI adapter requires exactly one clean absolute executable path; argv options are unsupported")
			}
			decoded, err := hex.DecodeString(a.ExecutableSHA256)
			if err != nil || len(decoded) != 32 || strings.ToLower(a.ExecutableSHA256) != a.ExecutableSHA256 {
				return errors.New("adapter executable requires lowercase SHA256")
			}
		default:
			return errors.New("unsupported adapter kind")
		}
	}
	for name, instance := range c.Instances {
		if !ExecutionName.MatchString(name) {
			return errors.New("invalid instance name")
		}
		adapter, ok := c.Adapters[instance.Adapter]
		if !ok {
			return errors.New("instance references unknown adapter")
		}
		if len(instance.CredentialRefs) > 8 {
			return errors.New("too many credential references")
		}
		if (adapter.Kind == "mock" || adapter.Kind == "ollama_http") && len(instance.CredentialRefs) != 0 {
			return errors.New("mock instance cannot use credentials")
		}
		if (adapter.Kind == "claude_cli" || adapter.Kind == "codex_cli" || adapter.Kind == "agy_cli") && len(instance.CredentialRefs) != 1 {
			return errors.New("CLI adapter requires exactly one explicit credential reference")
		}
		for _, ref := range instance.CredentialRefs {
			if ref.Kind == "host_session" && ((adapter.Kind == "claude_cli" && ref.Name == "claude") || (adapter.Kind == "codex_cli" && ref.Name == "codex") || (adapter.Kind == "agy_cli" && ref.Name == "agy")) {
				continue
			}
			if ref.Kind == "env" && executionEnvName.MatchString(ref.Name) {
				continue
			}
			return errors.New("unsupported credential reference; values are never accepted")
		}
	}
	for name, p := range c.Profiles {
		if !ExecutionName.MatchString(name) || !ExecutionModel.MatchString(p.Model) {
			return errors.New("invalid profile name or model selector")
		}
		instance, ok := c.Instances[p.Instance]
		if !ok {
			return errors.New("profile references unknown instance")
		}
		a := c.Adapters[instance.Adapter]
		if len(p.Tools) != 0 {
			return errors.New("tools are unsupported by current execution profiles")
		}
		if p.Limits.InputBytes < 1 || p.Limits.InputBytes > model.MaxInputBytes || p.Limits.OutputBytes < 1 || p.Limits.OutputBytes > model.MaxOutputBytes {
			return errors.New("profile byte limits exceed runtime capabilities")
		}
		if p.Limits.MaxOutputTokens < 0 || p.Limits.MaxOutputTokens > OllamaMaxOutputTokens {
			return errors.New("max_output_tokens must be 1..512 when set")
		}
		if p.Limits.MaxOutputTokens != 0 && a.Kind != "ollama_http" {
			return errors.New("max_output_tokens requires the local Ollama adapter")
		}
		if p.TimeoutSeconds < 0 || p.TimeoutSeconds > 3600 {
			return errors.New("profile timeout_seconds must be between 1 and 3600 when set")
		}
		amount, err := ExecutionBudgetMicros(p.OperationBudgetUSD)
		if err != nil {
			return err
		}
		if a.Kind != "ollama_http" && p.ModelDigest != "" {
			return errors.New("model digest only supported by Ollama")
		}
		if a.Kind == "ollama_http" {
			if p.Effort != "none" || amount == 0 || !validOllamaDigest(p.ModelDigest) || strings.Contains(p.Model, "cloud") {
				return errors.New("Ollama requires none effort, positive reservation, local model and SHA256 digest")
			}
		} else if a.Kind == "mock" {
			if p.Model != "mock" || p.Effort != "none" || amount != 0 {
				return errors.New("mock requires model mock, effort none, and zero budget")
			}
		} else {
			if a.Kind == "codex_cli" && p.Effort != "low" {
				return errors.New("current codex adapter supports low effort only")
			}
			if a.Kind == "agy_cli" && p.Effort != "low" {
				return errors.New("agy adapter supports low effort only; choose the effort through the agy model id (for example gemini-3.8-flash-low)")
			}
			if p.Effort != "low" && p.Effort != "medium" && p.Effort != "high" {
				return errors.New("unsupported claude effort")
			}
			if amount == 0 {
				return errors.New("claude profile requires positive operation budget")
			}
		}
		supported := map[string]bool{}
		for _, cap := range ExecutionProfileCapabilities(a.Kind, p) {
			supported[cap] = true
		}
		seen := map[string]bool{}
		for _, cap := range p.RequiredCapabilities {
			if !supported[cap] || seen[cap] {
				return errors.New("unsupported or duplicate required capability")
			}
			seen[cap] = true
		}
	}
	if _, ok := c.Profiles[c.DefaultProfile]; !ok {
		return errors.New("default profile is unknown")
	}
	return nil
}

func ReadExecutionCatalog(path string) (model.ExecutionCatalog, error) {
	var empty model.ExecutionCatalog
	f, err := OpenContextFile(path, os.O_RDONLY, 0)
	if err != nil {
		return empty, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > ExecutionCatalogLimit {
		return empty, errors.New("catalog must be a bounded regular file")
	}
	raw, err := io.ReadAll(io.LimitReader(f, ExecutionCatalogLimit+1))
	if err != nil {
		return empty, err
	}
	return parseExecutionCatalog(raw)
}
func ExecutionDigest(value any) string {
	raw, _ := json.Marshal(value)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func ResolveExecutionProfile(path, name string) (model.ExecutionBinding, error) {
	var result model.ExecutionBinding
	c, err := ReadExecutionCatalog(path)
	if err != nil {
		return result, err
	}
	return ResolveExecutionCatalog(c, path, name)
}

func ResolveExecutionCatalog(c model.ExecutionCatalog, path, name string) (model.ExecutionBinding, error) {
	var result model.ExecutionBinding
	if err := ValidateExecutionCatalog(c); err != nil {
		return result, err
	}
	if name == "" {
		name = c.DefaultProfile
	}
	p, ok := c.Profiles[name]
	if !ok {
		return result, errors.New("unknown profile")
	}
	instance := c.Instances[p.Instance]
	adapter := c.Adapters[instance.Adapter]
	verified := false
	if adapter.Kind == "claude_cli" || adapter.Kind == "codex_cli" || adapter.Kind == "agy_cli" {
		f, err := OpenContextFile(adapter.Command[0], os.O_RDONLY, 0)
		if err != nil {
			return result, err
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
			return result, errors.New("adapter must be an executable regular file")
		}
		if info.Size() > ExecutionExecutableLimit {
			return result, errors.New("adapter executable exceeds 512 MiB limit")
		}
		hash := sha256.New()
		n, err := io.Copy(hash, io.LimitReader(f, ExecutionExecutableLimit+1))
		if err != nil || n > ExecutionExecutableLimit || hex.EncodeToString(hash.Sum(nil)) != adapter.ExecutableSHA256 {
			return result, errors.New("adapter executable SHA256 mismatch")
		}
		verified = true
	}
	amount, _ := ExecutionBudgetMicros(p.OperationBudgetUSD)
	result = model.ExecutionBinding{Schema: ExecutionBindingSchema, CatalogPath: path, CatalogSHA256: ExecutionDigest(c), ProfileName: name, Profile: p, InstanceName: p.Instance, Instance: instance, AdapterName: instance.Adapter, Adapter: adapter, SupportedCapabilities: ExecutionProfileCapabilities(adapter.Kind, p), CapabilityEvidence: "declared_contract_not_live_probe", ModelResolution: "requested_only", ExecutableVerified: verified, OperationBudgetMicros: amount}
	result.Digest = ExecutionDigest(result)
	return result, nil
}
