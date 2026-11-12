package managed

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

const ollamaSuccess = `{"model":"fixture:latest","done":true,"done_reason":"stop","response":"{\"answer\":\"ok\"}","prompt_eval_count":4,"eval_count":2}`

func ollamaFixture(t *testing.T, generate http.HandlerFunc) (model.ExecutionBinding, model.WorkRequest, string, string, *atomic.Int32) {
	t.Helper()
	b, req, dir, prefix := runfixture.ManagedAdapterFixture(t, "")
	calls := &atomic.Int32{}
	digest := strings.Repeat("a", 64)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("unexpected credential")
		}
		switch r.URL.Path {
		case "/api/tags":
			fmt.Fprintf(w, `{"models":[{"name":"fixture:latest","digest":%q}]}`, digest)
		case "/api/show":
			fmt.Fprint(w, `{"details":{"format":"gguf"},"capabilities":["completion"]}`)
		case "/api/generate":
			calls.Add(1)
			generate(w, r)
		default:
			t.Error("unexpected route", r.URL.Path)
			http.Error(w, "bad", 400)
		}
	}))
	t.Cleanup(server.Close)
	b.Adapter = model.ExecutionAdapter{Kind: "ollama_http", Protocol: model.ProtocolVersion, Endpoint: server.URL}
	b.Instance.CredentialRefs = []model.ExecutionCredentialRef{}
	b.Profile.Model = "fixture:latest"
	b.Profile.ModelDigest = digest
	b.Profile.Effort = "none"
	req.Task.Effort = "none"
	b.SupportedCapabilities = launch.ExecutionCapabilities("ollama_http")
	b.ExecutableVerified = false
	b.Digest = ""
	b.Digest = launch.ExecutionDigest(b)
	return b, req, dir, prefix, calls
}
func TestManagedOllamaDispatch(t *testing.T) {
	b, req, dir, prefix, calls := ollamaFixture(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			t.Error("bad request")
		}
		if r.Method != "POST" || body["stream"] != false || body["think"] != false || body["format"] == nil || body["tools"] != nil {
			t.Error("unsafe generation", body)
		}
		options, _ := body["options"].(map[string]any)
		if options["num_predict"] != float64(512) {
			t.Error("missing generation bound")
		}
		fmt.Fprint(w, ollamaSuccess)
	})
	response, err := managedWork(context.Background(), dir, prefix, req, b)
	if err != nil || response.Output["answer"] != "ok" || !response.CostUnknown || calls.Load() != 1 || response.Usage.OutputTokens != 2 {
		t.Fatalf("result %+v %v", response, err)
	}
}
func TestManagedOllamaNoDispatchOnDigestMismatch(t *testing.T) {
	b, req, dir, prefix, calls := ollamaFixture(t, func(w http.ResponseWriter, r *http.Request) { t.Error("must not dispatch") })
	b.Profile.ModelDigest = strings.Repeat("b", 64)
	b.Digest = ""
	b.Digest = launch.ExecutionDigest(b)
	response, err := managedWork(context.Background(), dir, prefix, req, b)
	if err == nil || response.Outcome != model.OutcomeNotStarted || calls.Load() != 0 || response.CostUnknown {
		t.Fatalf("result %+v %v", response, err)
	}
}
func TestManagedOllamaCancellationUncertain(t *testing.T) {
	started := make(chan struct{})
	b, req, dir, prefix, calls := ollamaFixture(t, func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		close(started)
		<-r.Context().Done()
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { <-started; cancel() }()
	response, err := managedWork(ctx, dir, prefix, req, b)
	if err == nil || response.Outcome != model.OutcomeUncertain || !response.CostUnknown || calls.Load() != 1 {
		t.Fatalf("result %+v %v", response, err)
	}
}
func TestManagedOllamaParser(t *testing.T) {
	task := model.Task{ID: "node", Requires: []string{"answer"}}
	for _, raw := range []string{
		strings.Replace(ollamaSuccess, `"done":true`, `"done":false`, 1),
		strings.Replace(ollamaSuccess, `"done_reason":"stop"`, `"done_reason":"length"`, 1),
		strings.Replace(ollamaSuccess, `"eval_count":2`, `"eval_count":-1`, 1),
		strings.Replace(ollamaSuccess, `"done":true`, `"done":true,"done":true`, 1),
		strings.Replace(ollamaSuccess, `"done":true`, `"Done":true`, 1),
		strings.Replace(ollamaSuccess, `"done":true`, `"done":true,"tool_calls":[{}]`, 1),
		strings.Replace(ollamaSuccess, `"done":true`, `"done":true,"remote_host":"remote"`, 1),
		strings.Replace(ollamaSuccess, `"model":"fixture:latest"`, `"model":"other"`, 1),
		ollamaSuccess + `{}`,
	} {
		if _, _, err := ParseManagedOllama([]byte(raw), task, "fixture:latest"); err == nil {
			t.Error("accepted", raw)
		}
	}
}
func TestManagedOllamaEndpointAndOutputBound(t *testing.T) {
	for _, endpoint := range []string{"http://localhost:11434", "http://127.0.0.2:11434", "https://127.0.0.1:11434", "http://127.0.0.1:11434/", "http://user@127.0.0.1:11434", "http://127.0.0.1:11434?x=y", "http://127.0.0.1:011434"} {
		if launch.ValidateOllamaEndpoint(endpoint) == nil {
			t.Error("accepted endpoint", endpoint)
		}
	}
	b, req, dir, prefix, _ := ollamaFixture(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, strings.Repeat("x", 200)) })
	b.Profile.Limits.OutputBytes = 100
	b.Digest = ""
	b.Digest = launch.ExecutionDigest(b)
	response, err := managedWork(context.Background(), dir, prefix, req, b)
	if err == nil || response.Outcome != model.OutcomeUncertain {
		t.Fatalf("result %+v %v", response, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	time.Sleep(time.Millisecond)
	response, err = managedWork(ctx, dir, prefix, req, b)
	if err == nil || response.Outcome != model.OutcomeNotStarted {
		t.Fatalf("canceled %+v %v", response, err)
	}
}

func TestManagedOllamaPreflightAndPostflightRejections(t *testing.T) {
	for _, mode := range []string{"cloud", "no_completion", "redirect", "post_digest"} {
		t.Run(mode, func(t *testing.T) {
			b, req, dir, prefix, _ := ollamaFixture(t, func(w http.ResponseWriter, r *http.Request) {})
			var generated atomic.Int32
			var redirected atomic.Int32
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1) }))
			defer target.Close()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/tags":
					if mode == "redirect" {
						http.Redirect(w, r, target.URL, http.StatusFound)
						return
					}
					digest := b.Profile.ModelDigest
					if mode == "post_digest" && generated.Load() > 0 {
						digest = strings.Repeat("b", 64)
					}
					fmt.Fprintf(w, `{"models":[{"name":"fixture:latest","digest":%q}]}`, digest)
				case "/api/show":
					if mode == "cloud" {
						fmt.Fprint(w, `{"details":{"format":"gguf"},"capabilities":["completion"],"remote_host":"https://remote.invalid"}`)
						return
					}
					if mode == "no_completion" {
						fmt.Fprint(w, `{"details":{"format":"gguf"},"capabilities":["embedding"]}`)
						return
					}
					fmt.Fprint(w, `{"details":{"format":"gguf"},"capabilities":["completion"]}`)
				case "/api/generate":
					generated.Add(1)
					fmt.Fprint(w, ollamaSuccess)
				default:
					t.Error("unexpected route")
				}
			}))
			defer server.Close()
			b.Adapter.Endpoint = server.URL
			b.Digest = ""
			b.Digest = launch.ExecutionDigest(b)
			response, err := managedWork(context.Background(), dir, prefix, req, b)
			if err == nil {
				t.Fatal("accepted", mode)
			}
			if mode == "post_digest" {
				if response.Outcome != model.OutcomeUncertain || !response.CostUnknown || generated.Load() != 1 {
					t.Fatalf("postflight %+v", response)
				}
			} else if response.Outcome != model.OutcomeNotStarted || generated.Load() != 0 || redirected.Load() != 0 {
				t.Fatalf("preflight %+v", response)
			}
		})
	}
}

func TestOllamaExecutionCatalogValidation(t *testing.T) {
	b, _, _, _, _ := ollamaFixture(t, func(w http.ResponseWriter, r *http.Request) {})
	catalog := func() model.ExecutionCatalog {
		return model.ExecutionCatalog{Schema: launch.ExecutionCatalogSchema, Adapters: map[string]model.ExecutionAdapter{b.AdapterName: b.Adapter}, Instances: map[string]model.ExecutionInstance{b.InstanceName: b.Instance}, Profiles: map[string]model.ExecutionProfile{b.ProfileName: b.Profile}, DefaultProfile: b.ProfileName}
	}
	c := catalog()
	if err := launch.ValidateExecutionCatalog(c); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"credentials", "digest", "effort", "zero_budget", "command", "endpoint", "history"} {
		t.Run(mode, func(t *testing.T) {
			c := catalog()
			a := c.Adapters[b.AdapterName]
			p := c.Profiles[b.ProfileName]
			i := c.Instances[b.InstanceName]
			switch mode {
			case "credentials":
				i.CredentialRefs = []model.ExecutionCredentialRef{{Kind: "env", Name: "SECRET"}}
			case "digest":
				p.ModelDigest = "bad"
			case "effort":
				p.Effort = "low"
			case "zero_budget":
				p.OperationBudgetUSD = "0"
			case "command":
				a.Command = []string{"/bin/sh"}
			case "endpoint":
				a.Endpoint = "https://remote.example"
			case "history":
				p.RequiredCapabilities = []string{"explicit_history"}
			}
			c.Adapters[b.AdapterName] = a
			c.Profiles[b.ProfileName] = p
			c.Instances[b.InstanceName] = i
			if launch.ValidateExecutionCatalog(c) == nil {
				t.Fatal("accepted", mode)
			}
		})
	}
}

func TestOllamaRequiresVersionSix(t *testing.T) {
	b, _, _, _, _ := ollamaFixture(t, func(w http.ResponseWriter, r *http.Request) {})
	catalog := model.ExecutionCatalog{Schema: launch.ExecutionCatalogSchema, Adapters: map[string]model.ExecutionAdapter{b.AdapterName: b.Adapter}, Instances: map[string]model.ExecutionInstance{b.InstanceName: b.Instance}, Profiles: map[string]model.ExecutionProfile{b.ProfileName: b.Profile}, DefaultProfile: b.ProfileName}
	p := model.Plan{APIVersion: launch.ArtifactPlanVersion, Execution: &model.ManagedExecutionPlan{Catalog: catalog, AllowedProfiles: []string{b.ProfileName}, RunBudgetUSD: "1", CostPolicy: "retain_unknown"}}
	if err := checkpoint.ValidateManagedPlan(p); err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{"swarm/v3", "swarm/v4", "swarm/v5"} {
		p.APIVersion = version
		if version == "swarm/v3" {
			p.Execution.CostPolicy = ""
		} else {
			p.Execution.CostPolicy = "retain_unknown"
		}
		if checkpoint.ValidateManagedPlan(p) == nil {
			t.Fatal("accepted Ollama for", version)
		}
	}
}
