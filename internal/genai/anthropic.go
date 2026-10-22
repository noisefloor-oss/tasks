package genai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	// DefaultAnthropicModel is retained only for the preference-less
	// compatibility path. Configured projects always provide an explicit model.
	DefaultAnthropicModel = "claude-sonnet-5"
	defaultBaseURL        = "https://api.anthropic.com"
	apiVersion            = "2023-06-01"
	maxAPIResponse        = 8 << 20

	// maxTokens is generous: n tasks with details comfortably fit, and
	// the model stops on its own long before this.
	maxTokens = 16000
)

// Anthropic calls the Messages API directly with net/http (stdlib
// only, no SDK dependency). It is the v1 driver behind Provider.
type Anthropic struct {
	APIKey  string
	Model   string
	BaseURL string
	Client  *http.Client
}

// NewAnthropicConfigured builds the in-process adapter from a caller-selected
// model. Selector environment variables are deliberately ignored; only the
// adapter-managed authentication secret is read at invocation time.
func NewAnthropicConfigured(model string) (*Anthropic, error) {
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		return nil, errors.New(
			"ANTHROPIC_API_KEY is not set; generate calls the Anthropic API and needs a key (export ANTHROPIC_API_KEY=<key>)")
	}
	if err := validateModelArgument(model, true); err != nil {
		return nil, fmt.Errorf("anthropic-api: %w", err)
	}
	return &Anthropic{
		APIKey:  key,
		Model:   model,
		BaseURL: defaultBaseURL,
		Client:  &http.Client{Timeout: 5 * time.Minute},
	}, nil
}

// messagesRequest is the subset of the Messages API request we send.
type messagesRequest struct {
	Model     string           `json:"model"`
	MaxTokens int              `json:"max_tokens"`
	System    string           `json:"system"`
	Messages  []requestMessage `json:"messages"`
}

type requestMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// messagesResponse is the subset of the Messages API response we read.
type messagesResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StopReason string `json:"stop_reason"`
	Model      string `json:"model"`
}

// GenerateTasks asks the model for at most n task drafts and parses
// the reply defensively: non-JSON is rejected, surplus drafts are
// dropped, and every draft is validated before it is returned.
func (a *Anthropic) GenerateTasks(ctx context.Context, prdText string, n int) (Result, error) {
	n = clampDraftCount(n)
	if err := validateModelArgument(a.Model, true); err != nil {
		return Result{}, fmt.Errorf("anthropic-api: %w", err)
	}

	body, err := json.Marshal(messagesRequest{
		Model:     a.Model,
		MaxTokens: maxTokens,
		System:    draftSystemPrompt,
		Messages: []requestMessage{{
			Role:    "user",
			Content: draftUserPrompt(n, prdText),
		}},
	})
	if err != nil {
		return Result{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(a.BaseURL, "/")+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("x-api-key", a.APIKey)
	req.Header.Set("anthropic-version", apiVersion)
	req.Header.Set("content-type", "application/json")

	resp, err := a.Client.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("calling Anthropic API: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxAPIResponse+1))
	if err != nil {
		return Result{}, fmt.Errorf("reading Anthropic API response: %w", err)
	}
	if len(raw) > maxAPIResponse {
		return Result{}, fmt.Errorf("Anthropic API response exceeded %d bytes", maxAPIResponse)
	}

	if resp.StatusCode != http.StatusOK {
		return Result{}, fmt.Errorf("Anthropic API: HTTP %d", resp.StatusCode)
	}

	var msg messagesResponse
	if err := json.Unmarshal(raw, &msg); err != nil {
		return Result{}, fmt.Errorf("Anthropic API returned a malformed response body: %w", err)
	}
	switch msg.StopReason {
	case "refusal":
		return Result{}, errors.New("the model declined to process this PRD (stop_reason: refusal)")
	case "max_tokens":
		return Result{}, errors.New("model output was truncated (stop_reason: max_tokens); try fewer tasks")
	}

	var text strings.Builder
	for _, block := range msg.Content {
		if block.Type == "text" {
			text.WriteString(block.Text)
		}
	}

	drafts, err := finishDrafts(text.String(), n)
	if err != nil {
		return Result{}, err
	}
	return Result{Drafts: drafts, ResolvedModel: msg.Model}, nil
}
