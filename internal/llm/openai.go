package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// openAIClient calls an OpenAI-compatible Chat Completions endpoint with a
// strict JSON-schema response format. It needs OPENAI_API_KEY; OPENAI_BASE_URL
// points it at another compatible server.
type openAIClient struct {
	model   string
	baseURL string
	key     string
	http    *http.Client
}

func newOpenAI(cfg Config) (*openAIClient, error) {
	if cfg.Model == "" {
		return nil, fmt.Errorf("backend openai needs --model (Stele does not pick an OpenAI model for you)")
	}
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		return nil, fmt.Errorf("backend openai needs OPENAI_API_KEY")
	}
	base := strings.TrimRight(os.Getenv("OPENAI_BASE_URL"), "/")
	if base == "" {
		base = "https://api.openai.com/v1"
	}
	return &openAIClient{model: cfg.Model, baseURL: base, key: key, http: &http.Client{Timeout: 15 * time.Minute}}, nil
}

func (c *openAIClient) Name() string { return "openai:" + c.model }

type oaMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func (c *openAIClient) Complete(ctx context.Context, req Request) (*Reply, error) {
	msgs := []oaMessage{{Role: "system", Content: req.System}}
	for _, t := range req.Turns {
		msgs = append(msgs, oaMessage{Role: t.Role, Content: t.Text})
	}
	body, err := json.Marshal(map[string]any{
		"model":    c.model,
		"messages": msgs,
		"response_format": map[string]any{
			"type": "json_schema",
			"json_schema": map[string]any{
				"name":   "stele_reply",
				"strict": true,
				"schema": json.RawMessage(req.Schema),
			},
		},
	})
	if err != nil {
		return nil, err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	hreq.Header.Set("Authorization", "Bearer "+c.key)
	hreq.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(hreq)
	if err != nil {
		return nil, fmt.Errorf("openai: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("openai: HTTP %d: %s", resp.StatusCode, tail(string(raw)))
	}
	var out struct {
		Model   string `json:"model"`
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content string `json:"content"`
				Refusal string `json:"refusal"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			Prompt     int64 `json:"prompt_tokens"`
			Completion int64 `json:"completion_tokens"`
			Details    struct {
				Cached int64 `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("openai: unreadable response: %w", err)
	}
	if len(out.Choices) == 0 {
		return nil, fmt.Errorf("openai: no choices in response")
	}
	ch := out.Choices[0]
	switch {
	case ch.Message.Refusal != "":
		return nil, fmt.Errorf("openai: model declined the request: %s", ch.Message.Refusal)
	case ch.FinishReason == "length":
		return nil, fmt.Errorf("openai: reply hit the token limit")
	}
	return &Reply{
		JSON:  ch.Message.Content,
		Model: out.Model,
		Usage: Usage{
			InputTokens:     out.Usage.Prompt - out.Usage.Details.Cached,
			CacheReadTokens: out.Usage.Details.Cached,
			OutputTokens:    out.Usage.Completion,
		},
	}, nil
}
