package executor

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestInstalledAgentDoesNotClaimAuthentication(t *testing.T) {
	dir := t.TempDir()
	for _, host := range []string{"claude", "codex", "gemini", "agy"} {
		if err := os.WriteFile(filepath.Join(dir, host), []byte("#!/bin/sh\necho 'agent 9.9.9'\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	for _, agent := range detectAgents() {
		if agent.MainAgent && (!agent.Available || agent.Authentication != "unchecked" || !strings.Contains(agent.Detail, "not been checked")) {
			t.Fatalf("installed CLI must not imply authenticated readiness: %+v", agent)
		}
	}
}

func TestAgentsRolesAndBuiltInMock(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no CLIs installed
	byID := map[string]agentStatus{}
	for _, a := range detectAgents() {
		byID[a.ID] = a
	}
	for _, id := range []string{"claude", "codex", "agy"} {
		if a := byID[id]; !a.MainAgent || !a.SwarmWorker || a.Profile != id+":MODEL" || a.Available {
			t.Fatalf("%s: want main agent and swarm worker, unavailable without the CLI: %+v", id, a)
		}
	}
	for _, id := range []string{"gemini"} {
		if a := byID[id]; !a.MainAgent || a.SwarmWorker || a.Profile != "" || a.Available {
			t.Fatalf("%s: want main agent only (not built-in swarm worker), unavailable without the CLI: %+v", id, a)
		}
	}
	if a := byID["ollama"]; a.MainAgent || !a.SwarmWorker {
		t.Fatalf("ollama is a swarm worker only: %+v", a)
	}
	if a := byID["mock"]; !a.Available || a.MainAgent {
		t.Fatalf("mock workers are always available and never the main agent: %+v", a)
	}
}

func TestAgentModelCatalog(t *testing.T) {
	models, err := readAgentModels(strings.NewReader(`{"models":[{"name":"zeta:latest"},{"name":"registry.example:443/library/alpha:1"},{"name":"zeta:latest"}]}`))
	if err != nil || !reflect.DeepEqual(models, []string{"registry.example:443/library/alpha:1", "zeta:latest"}) {
		t.Fatalf("want sorted, unique installed models, got %v, %v", models, err)
	}
	for _, input := range []string{
		`{`, `null`, `{}`, `{"models":null}`, `{"models":{}}`,
		`{"models":[]} trailing`, `{"models":[{}]}`, `{"models":[{"name":5}]}`,
		`{"models":[{"name":""}]}`, `{"models":[{"name":"a b"}]}`,
		`{"models":[{"name":"model\nname"}]}`, `{"models":[{"name":"../outside"}]}`,
		`{"models":[{"name":"a;echo"}]}`, `{"models":[{"name":"` + strings.Repeat("a", 129) + `"}]}`,
		strings.Repeat(" ", agentModelCatalogLimit+1),
		`{"models":[` + strings.Repeat(`{"name":"a"},`, 1024) + `{"name":"a"}]}`,
	} {
		if _, err := readAgentModels(strings.NewReader(input)); err == nil {
			t.Fatalf("accepted malformed or oversized catalog (%d bytes)", len(input))
		}
	}
	if models, err := readAgentModels(strings.NewReader(`{"models":[]}`)); err != nil || len(models) != 0 {
		t.Fatalf("valid empty catalog: %v, %v", models, err)
	}
}

type agentDiscoveryTransport func(*http.Request) (*http.Response, error)

func (f agentDiscoveryTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type agentDiscoveryBody struct {
	io.Reader
	closed bool
}

func (b *agentDiscoveryBody) Close() error { b.closed = true; return nil }

func TestOllamaAgentDiscovery(t *testing.T) {
	for _, test := range []struct {
		name, body, detail string
		status             int
		available          bool
	}{
		{"installed models", `{"models":[{"name":"code:latest"}]}`, "1 installed model", 200, true},
		{"empty", `{"models":[]}`, "no models are installed", 200, false},
		{"bad JSON", `{`, "could not be read", 200, false},
		{"missing list", `{}`, "could not be read", 200, false},
		{"error response", `{"models":[{"name":"code:latest"}]}`, "HTTP 503", 503, false},
		{"redirect", "", "HTTP 302", 302, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := &agentDiscoveryBody{Reader: strings.NewReader(test.body)}
			client := &http.Client{Transport: agentDiscoveryTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.String() != "http://local.invalid/api/tags" {
					t.Fatalf("wrong discovery endpoint: %s", r.URL)
				}
				return &http.Response{StatusCode: test.status, Body: body, Header: make(http.Header)}, nil
			})}
			a := detectOllamaAgent(client, "http://local.invalid/api/tags", true)
			if a.Available != test.available || !strings.Contains(a.Detail, test.detail) || a.MainAgent || !a.SwarmWorker || !body.closed {
				t.Fatalf("unexpected discovery result/cleanup: %+v, body closed=%v", a, body.closed)
			}
			if a.Available && !reflect.DeepEqual(a.Models, []string{"code:latest"}) {
				t.Fatalf("installed names missing: %+v", a)
			}
			if !a.Available && len(a.Models) != 0 {
				t.Fatalf("unavailable provider advertises models: %+v", a)
			}
		})
	}
	client := &http.Client{Transport: agentDiscoveryTransport(func(*http.Request) (*http.Response, error) { return nil, errors.New("offline") })}
	for _, installed := range []bool{false, true} {
		a := detectOllamaAgent(client, "http://local.invalid/api/tags", installed)
		want := "Optional. Install"
		if installed {
			want = "Not running."
		}
		if a.Available || !strings.HasPrefix(a.Detail, want) {
			t.Fatalf("offline installed=%v: %+v", installed, a)
		}
		encoded, err := json.Marshal(a)
		if err != nil || strings.Contains(string(encoded), `"models"`) {
			t.Fatalf("optional model catalog leaked when absent: %s, %v", encoded, err)
		}
	}
}

func TestAgyMinimumVersionAndStatus(t *testing.T) {
	for _, tc := range []struct {
		name          string
		output        string
		wantAvailable bool
		wantStatus    string
		wantDetailSub string
	}{
		{"ok_exact", "1.5.3", true, "ok", "Installed; authentication has not been checked."},
		{"ok_newer", "1.6.0", true, "ok", "Installed; authentication has not been checked."},
		{"too_old", "1.5.2", false, "too_old", "too old: found 1.5.2, need >= 1.5.3"},
		{"too_old_minor", "1.4.99", false, "too_old", "too old: found 1.4.99, need >= 1.5.3"},
		{"unreadable", "unreadable-version", false, "unknown_version", "agy version unreadable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			script := fmt.Sprintf("#!/bin/sh\necho %q\n", tc.output)
			if err := os.WriteFile(filepath.Join(dir, "agy"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir)
			var found *agentStatus
			for _, a := range detectAgents() {
				if a.ID == "agy" {
					copy := a
					found = &copy
					break
				}
			}
			if found == nil {
				t.Fatal("agy agent not found in detectAgents")
			}
			if found.Available != tc.wantAvailable {
				t.Errorf("Available = %v, want %v", found.Available, tc.wantAvailable)
			}
			if found.Status != tc.wantStatus {
				t.Errorf("Status = %q, want %q", found.Status, tc.wantStatus)
			}
			if !strings.Contains(found.Detail, tc.wantDetailSub) {
				t.Errorf("Detail %q does not contain %q", found.Detail, tc.wantDetailSub)
			}
		})
	}
}
