package managed

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/grainulation/irrigator/internal/artifactstore"
	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/model"
)

// Zero selects the fixed default generation ceiling. A configured ceiling
// is enforced by the trusted local server's num_predict option; it is not a cap
// on prompt evaluation, remote providers, aggregate token use or billing.
func ollamaOutputTokenLimit(p model.ExecutionProfile) (int64, error) {
	limit := p.Limits.MaxOutputTokens
	if limit == 0 {
		return launch.OllamaMaxOutputTokens, nil
	}
	if limit < 1 || limit > launch.OllamaMaxOutputTokens {
		return 0, errors.New("Ollama max_output_tokens must be 1..512")
	}
	return limit, nil
}

func ollamaJSON(raw []byte) (map[string]any, error) {
	return codexObject(raw, "models", "model", "name", "digest", "details", "format", "capabilities", "remote_model", "remote_host", "tool_calls", "response", "done", "done_reason", "prompt_eval_count", "eval_count", "error")
}

func ollamaRequest(ctx context.Context, client *http.Client, endpoint, route string, body []byte, limit int64) ([]byte, error) {
	method := http.MethodGet
	if body != nil {
		method = http.MethodPost
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint+route, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, errors.New("Ollama HTTP request failed")
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil || int64(len(raw)) > limit {
		return nil, errors.New("Ollama response exceeds bound or is incomplete")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New("Ollama HTTP status rejected")
	}
	return raw, nil
}
func ollamaLocalModel(ctx context.Context, client *http.Client, b model.ExecutionBinding) error {
	raw, err := ollamaRequest(ctx, client, b.Adapter.Endpoint, "/api/tags", nil, launch.ExecutionCatalogLimit)
	if err != nil {
		return err
	}
	object, err := ollamaJSON(raw)
	if err != nil {
		return err
	}
	models, ok := object["models"].([]any)
	if !ok {
		return errors.New("Ollama inventory missing")
	}
	matches := 0
	for _, v := range models {
		m, ok := v.(map[string]any)
		if !ok {
			return errors.New("invalid Ollama inventory")
		}
		if m["name"] == b.Profile.Model {
			matches++
			if m["digest"] != b.Profile.ModelDigest {
				return errors.New("Ollama model digest mismatch")
			}
		}
	}
	if matches != 1 {
		return errors.New("Ollama requires exactly one already installed pinned model")
	}
	body, _ := json.Marshal(map[string]any{"model": b.Profile.Model})
	raw, err = ollamaRequest(ctx, client, b.Adapter.Endpoint, "/api/show", body, launch.ExecutionCatalogLimit)
	if err != nil {
		return err
	}
	object, err = ollamaJSON(raw)
	if err != nil {
		return err
	}
	for _, key := range []string{"remote_host", "remote_model"} {
		if value, exists := object[key]; exists && value != "" {
			return errors.New("remote Ollama model rejected")
		}
	}
	details, ok := object["details"].(map[string]any)
	if !ok || details["format"] != "gguf" {
		return errors.New("Ollama requires local GGUF model")
	}
	caps, ok := object["capabilities"].([]any)
	if !ok {
		return errors.New("Ollama missing capability evidence")
	}
	for _, c := range caps {
		if c == "completion" {
			return nil
		}
	}
	return errors.New("Ollama model lacks completion capability")
}
func managedOllamaWork(ctx context.Context, cell, prefix string, req model.WorkRequest, b model.ExecutionBinding, input []byte) (model.WorkResponse, error) {
	response := model.WorkResponse{Protocol: model.ProtocolVersion, RequestID: req.RequestID}
	fail := func(outcome string, err error) (model.WorkResponse, error) {
		response.Outcome = outcome
		response.Error = err.Error()
		return response, model.WithOutcome(outcome, err)
	}
	if err := launch.ValidateOllamaEndpoint(b.Adapter.Endpoint); err != nil {
		return fail(model.OutcomeNotStarted, err)
	}
	outputTokenLimit, err := ollamaOutputTokenLimit(b.Profile)
	if err != nil {
		return fail(model.OutcomeNotStarted, err)
	}
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true, DisableCompression: true, ResponseHeaderTimeout: 30 * time.Second, MaxResponseHeaderBytes: 16384}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("Ollama redirects forbidden") }}
	body, err := json.Marshal(map[string]any{"model": b.Profile.Model, "prompt": string(input), "system": ManagedSystemPrompt, "format": artifactstore.ModelSchemaForRequest(req), "stream": false, "think": false, "options": map[string]any{"num_predict": outputTokenLimit, "temperature": 0}})
	if err != nil || int64(len(body)) > b.Profile.Limits.InputBytes {
		return fail(model.OutcomeNotStarted, errors.New("Ollama request exceeds profile input bound"))
	}
	if err = ollamaLocalModel(ctx, client, b); err != nil {
		return fail(model.OutcomeNotStarted, err)
	}
	if err = os.WriteFile(prefix+".input.json", body, 0600); err != nil {
		return fail(model.OutcomeNotStarted, err)
	}
	if err = ctx.Err(); err != nil {
		return fail(model.OutcomeNotStarted, err)
	}
	response.CostUnknown = true
	raw, err := ollamaRequest(ctx, client, b.Adapter.Endpoint, "/api/generate", body, b.Profile.Limits.OutputBytes)
	if err != nil {
		return fail(model.OutcomeUncertain, err)
	}
	if err = os.WriteFile(prefix+".stdout.json", raw, 0600); err != nil {
		return fail(model.OutcomeUncertain, err)
	}
	output, usage, err := ParseManagedOllama(raw, req.Task, b.Profile.Model)
	if err != nil {
		return fail(model.OutcomeUncertain, err)
	}
	// Preserve reported usage as evidence even if the server violated the cap.
	// A violating completion is never accepted and its cost stays unknown.
	if usage != nil {
		response.Usage = usage.toTaskUsage(b.Profile.Model)
	}
	if usage != nil && usage.OutputTokens > outputTokenLimit {
		return fail(model.OutcomeUncertain, errors.New("Ollama reported generation above max_output_tokens"))
	}
	if err = ollamaLocalModel(ctx, client, b); err != nil {
		return fail(model.OutcomeUncertain, err)
	}
	response.Output = output
	response.Usage = usage.toTaskUsage(b.Profile.Model)
	response.Model = b.Profile.Model
	response.Outcome = model.OutcomeSettled
	return response, nil
}
func ParseManagedOllama(raw []byte, task model.Task, model string) (map[string]string, *codexTokenUsage, error) {
	o, err := ollamaJSON(raw)
	if err != nil {
		return nil, nil, err
	}
	if o["model"] != model || o["done"] != true || o["done_reason"] != "stop" {
		return nil, nil, errors.New("Ollama incomplete or mismatched result")
	}
	for _, key := range []string{"remote_host", "remote_model", "error"} {
		if value, ok := o[key]; ok && value != "" {
			return nil, nil, errors.New("Ollama remote/error result rejected")
		}
	}
	if value, ok := o["tool_calls"]; ok {
		v, ok := value.([]any)
		if !ok || len(v) != 0 {
			return nil, nil, errors.New("Ollama tool calls rejected")
		}
	}
	text, ok := o["response"].(string)
	if !ok {
		return nil, nil, errors.New("Ollama missing response")
	}
	if _, err = ollamaJSON([]byte(text)); err != nil {
		return nil, nil, err
	}
	output, err := artifactstore.NormalizeModelOutput(task, json.RawMessage(text))
	if err != nil {
		return nil, nil, err
	}
	counts := []int64{}
	for _, key := range []string{"prompt_eval_count", "eval_count"} {
		n, ok := o[key].(json.Number)
		if !ok {
			return nil, nil, errors.New("Ollama missing usage")
		}
		v, e := n.Int64()
		if e != nil || v < 0 {
			return nil, nil, errors.New("Ollama invalid usage")
		}
		counts = append(counts, v)
	}
	return output, &codexTokenUsage{InputTokens: counts[0], OutputTokens: counts[1]}, nil
}
