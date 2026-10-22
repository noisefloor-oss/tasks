package genai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// No test in this package (or anywhere in the repo) performs a live
// API call: the real driver is exercised only against httptest stubs.

func TestNewAnthropicConfiguredRequiresKey(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	_, err := NewAnthropicConfigured("configured-model")
	if err == nil {
		t.Fatal("configured Anthropic adapter with unset key should refuse")
	}
	if !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Errorf("error should name the missing variable: %v", err)
	}
}

func TestNewAnthropicConfiguredUsesFixedEndpointAndModel(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-test")
	a, err := NewAnthropicConfigured("claude-example-9")
	if err != nil {
		t.Fatal(err)
	}
	if a.Model != "claude-example-9" {
		t.Errorf("model = %q, want claude-example-9", a.Model)
	}
	if a.BaseURL != "https://api.anthropic.com" {
		t.Errorf("base URL = %q", a.BaseURL)
	}
	if a.Client == nil || a.Client.Timeout != 5*time.Minute {
		t.Errorf("client = %+v", a.Client)
	}
}

func TestNewAnthropicConfiguredIgnoresSelectorEnvironment(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-test")
	t.Setenv("NOISE_TASKS_MODEL", "ambient-model")
	t.Setenv("NOISETASK_MODEL", "conflicting-model")
	driver, err := NewAnthropicConfigured("configured-model")
	if err != nil {
		t.Fatal(err)
	}
	if driver.Model != "configured-model" {
		t.Fatalf("model = %q", driver.Model)
	}
}

// stubDriver points a real Anthropic driver at an httptest server whose
// per-request behavior is handler.
func stubDriver(t *testing.T, handler http.HandlerFunc) *Anthropic {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &Anthropic{
		APIKey:  "sk-test",
		Model:   "claude-sonnet-5",
		BaseURL: srv.URL,
		Client:  srv.Client(),
	}
}

// textResponse wraps text in a minimal Messages API success body.
func textResponse(text, stopReason string) string {
	body, _ := json.Marshal(map[string]any{
		"content":     []map[string]string{{"type": "text", "text": text}},
		"model":       "claude-resolved-20260812",
		"stop_reason": stopReason,
	})
	return string(body)
}

const validDraftsJSON = `[
  {"title": "Set up scaffolding", "description": "d1", "details": "x1", "priority": "high", "dependencies": []},
  {"title": "Build parser", "description": "d2", "details": "x2", "priority": "medium", "dependencies": [1]},
  {"title": "Wire CLI", "description": "d3", "details": "x3", "priority": "low", "dependencies": [1, 2]}
]`

func TestGenerateTasksHappyPath(t *testing.T) {
	var gotPath, gotKey, gotVersion string
	var gotReq messagesRequest
	driver := stubDriver(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		if err := json.NewDecoder(r.Body).Decode(&gotReq); err != nil {
			t.Errorf("request body not JSON: %v", err)
		}
		// Markdown fences around the JSON must be tolerated.
		fmt.Fprint(w, textResponse("```json\n"+validDraftsJSON+"\n```", "end_turn"))
	})

	result, err := driver.GenerateTasks(context.Background(), "the PRD text", 3)
	if err != nil {
		t.Fatalf("GenerateTasks: %v", err)
	}
	drafts := result.Drafts

	if gotPath != "/v1/messages" {
		t.Errorf("path = %q, want /v1/messages", gotPath)
	}
	if gotKey != "sk-test" {
		t.Errorf("x-api-key = %q", gotKey)
	}
	if gotVersion != "2023-06-01" {
		t.Errorf("anthropic-version = %q", gotVersion)
	}
	if gotReq.Model != "claude-sonnet-5" {
		t.Errorf("request model = %q", gotReq.Model)
	}
	if gotReq.MaxTokens < 4096 {
		t.Errorf("max_tokens = %d, want a generous budget", gotReq.MaxTokens)
	}
	if gotReq.System == "" || !strings.Contains(gotReq.System, "JSON") {
		t.Errorf("system prompt should demand JSON: %q", gotReq.System)
	}
	if len(gotReq.Messages) != 1 || !strings.Contains(gotReq.Messages[0].Content, "the PRD text") {
		t.Errorf("user message should carry the PRD: %+v", gotReq.Messages)
	}

	if len(drafts) != 3 {
		t.Fatalf("got %d drafts, want 3", len(drafts))
	}
	if drafts[0].Title != "Set up scaffolding" || drafts[0].Priority != "high" {
		t.Errorf("draft 1 = %+v", drafts[0])
	}
	if len(drafts[2].Dependencies) != 2 || drafts[2].Dependencies[0] != 1 || drafts[2].Dependencies[1] != 2 {
		t.Errorf("draft 3 dependencies = %v, want [1 2]", drafts[2].Dependencies)
	}
	if result.ResolvedModel != "claude-resolved-20260812" {
		t.Errorf("resolved model = %q", result.ResolvedModel)
	}
}

func TestGenerateTasksCapsSurplusDrafts(t *testing.T) {
	driver := stubDriver(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, textResponse(validDraftsJSON, "end_turn"))
	})
	// The model returned 3 drafts but only 2 were requested; the
	// surplus is dropped. Draft 2's dep [1] survives the truncation.
	result, err := driver.GenerateTasks(context.Background(), "prd", 2)
	if err != nil {
		t.Fatalf("GenerateTasks: %v", err)
	}
	drafts := result.Drafts
	if len(drafts) != 2 {
		t.Errorf("got %d drafts, want the requested cap of 2", len(drafts))
	}
}

func TestGenerateTasksRejectsMalformedModelOutput(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"prose instead of JSON", textResponse("Here are some great tasks for you!", "end_turn"),
			"JSON"},
		{"broken JSON array", textResponse(`[{"title": "a", `, "end_turn"), "JSON"},
		{"unknown fields", textResponse(`[{"title": "a", "id": "7"}]`, "end_turn"), "JSON"},
		{"empty array", textResponse(`[]`, "end_turn"), "no actionable"},
		{"empty title", textResponse(`[{"title": "  "}]`, "end_turn"), "empty title"},
		{"unknown priority", textResponse(`[{"title": "a", "priority": "urgent"}]`, "end_turn"),
			"unknown priority"},
		{"dep out of range", textResponse(`[{"title": "a", "dependencies": [5]}]`, "end_turn"),
			"does not exist"},
		{"self dep", textResponse(`[{"title": "a", "dependencies": [1]}]`, "end_turn"),
			"depends on itself"},
		{"refusal", textResponse("", "refusal"), "refusal"},
		{"truncated", textResponse(`[{"title"`, "max_tokens"), "max_tokens"},
		{"malformed response envelope", `{"content": "nope"`, "malformed"},
	}
	for _, tc := range cases {
		driver := stubDriver(t, func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, tc.body)
		})
		_, err := driver.GenerateTasks(context.Background(), "prd", 5)
		if err == nil {
			t.Errorf("%s: want an error, got none", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error = %q, want it to mention %q", tc.name, err, tc.want)
		}
	}
}

func TestGenerateTasksSurfacesAPIErrors(t *testing.T) {
	driver := stubDriver(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"type": "error", "error": {"type": "invalid_request_error", "message": "credential-shaped-secret"}}`)
	})
	_, err := driver.GenerateTasks(context.Background(), "prd", 5)
	if err == nil || !strings.Contains(err.Error(), "HTTP 400") || strings.Contains(err.Error(), "credential-shaped-secret") {
		t.Errorf("API error should report only the status class, got: %v", err)
	}

	// A non-200 without a JSON envelope still fails cleanly.
	driver = stubDriver(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprint(w, "upstream broke")
	})
	_, err = driver.GenerateTasks(context.Background(), "prd", 5)
	if err == nil || !strings.Contains(err.Error(), "502") {
		t.Errorf("plain HTTP error should report the status, got: %v", err)
	}
}

func TestGenerateTasksClampsRequestedN(t *testing.T) {
	var gotContent string
	driver := stubDriver(t, func(w http.ResponseWriter, r *http.Request) {
		var req messagesRequest
		json.NewDecoder(r.Body).Decode(&req)
		gotContent = req.Messages[0].Content
		fmt.Fprint(w, textResponse(`[{"title": "a"}]`, "end_turn"))
	})
	if _, err := driver.GenerateTasks(context.Background(), "prd", 100000); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotContent, fmt.Sprintf("at most %d tasks", MaxDrafts)) {
		t.Errorf("requested n should be clamped to %d: %q", MaxDrafts, gotContent)
	}
}

func TestGenerateTasksRejectsOversizedAPIResponse(t *testing.T) {
	driver := stubDriver(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, strings.Repeat("x", maxAPIResponse+1))
	})
	_, err := driver.GenerateTasks(context.Background(), "prd", 1)
	if err == nil || !strings.Contains(err.Error(), "response exceeded") {
		t.Fatalf("oversized response error = %v", err)
	}
}
