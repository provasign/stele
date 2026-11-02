package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
)

// apiClient calls the Anthropic API. Credentials resolve the SDK's way:
// ANTHROPIC_API_KEY, ANTHROPIC_AUTH_TOKEN, then an `ant auth login` profile.
type apiClient struct {
	client anthropic.Client
	model  string
	effort string
}

const defaultAPIModel = "claude-opus-5-5"

func newAPI(cfg Config) *apiClient {
	c := &apiClient{client: anthropic.NewClient(), model: cfg.Model, effort: cfg.Effort}
	if c.model == "" {
		c.model = defaultAPIModel
	}
	if c.effort == "" {
		c.effort = "high" // translation fidelity is the experiment; Opus 5.5 defaults to medium
	}
	return c
}

func (c *apiClient) Name() string { return "anthropic:" + c.model }

func (c *apiClient) Complete(ctx context.Context, req Request) (*Reply, error) {
	msgs := make([]anthropic.BetaMessageParam, len(req.Turns))
	for i, t := range req.Turns {
		block := anthropic.NewBetaTextBlock(t.Text)
		if t.Role == "assistant" {
			msgs[i] = anthropic.BetaMessageParam{Role: anthropic.BetaMessageParamRoleAssistant, Content: []anthropic.BetaContentBlockParamUnion{block}}
		} else {
			msgs[i] = anthropic.NewBetaUserMessage(block)
		}
	}
	params := anthropic.BetaMessageNewParams{
		Model:     anthropic.Model(c.model),
		MaxTokens: 64000,
		// The system prompt is stable across runs, so it is the cached prefix.
		System:   []anthropic.BetaTextBlockParam{{Text: req.System, CacheControl: anthropic.NewBetaCacheControlEphemeralParam()}},
		Messages: msgs,
		OutputConfig: anthropic.BetaOutputConfigParam{
			Effort: anthropic.BetaOutputConfigEffort(c.effort),
			Format: anthropic.BetaJSONOutputFormatParam{Schema: json.RawMessage(req.Schema)},
		},
		// A declined request is re-served server-side by the recommended fallback model.
		Fallbacks: anthropic.BetaFallbacksParamOfDefault(),
		Betas:     []anthropic.AnthropicBeta{"server-side-fallback-2026-07-01"},
	}
	stream := c.client.Beta.Messages.NewStreaming(ctx, params)
	msg := anthropic.BetaMessage{}
	for stream.Next() {
		if err := msg.Accumulate(stream.Current()); err != nil {
			return nil, err
		}
	}
	if err := stream.Err(); err != nil {
		return nil, fmt.Errorf("anthropic api: %w", err)
	}
	switch msg.StopReason {
	case anthropic.BetaStopReasonRefusal:
		return nil, fmt.Errorf("model declined the request: %s", msg.StopDetails.Explanation)
	case anthropic.BetaStopReasonMaxTokens:
		return nil, fmt.Errorf("reply hit max_tokens")
	}
	var text strings.Builder
	for _, b := range msg.Content {
		if t, ok := b.AsAny().(anthropic.BetaTextBlock); ok {
			text.WriteString(t.Text)
		}
	}
	u := msg.Usage
	return &Reply{
		JSON:  text.String(),
		Model: string(msg.Model),
		Usage: Usage{
			InputTokens: u.InputTokens, CacheWriteTokens: u.CacheCreationInputTokens,
			CacheReadTokens: u.CacheReadInputTokens, OutputTokens: u.OutputTokens,
			CostUSD: estimateCost(c.model, u),
		},
	}, nil
}

// estimateCost uses list prices per MTok; models without a known price report 0.
func estimateCost(model string, u anthropic.BetaUsage) float64 {
	prices := map[string][2]float64{ // input, output
		"claude-opus-5-5":   {4, 20},
		"claude-sonnet-5-5": {2, 10},
		"claude-haiku-5-5":  {0.10, 0.50},
		"claude-fable-5-1":  {10, 50},
	}
	p, ok := prices[model]
	if !ok {
		return 0
	}
	in := float64(u.InputTokens) + 1.25*float64(u.CacheCreationInputTokens) + 0.05*float64(u.CacheReadInputTokens)
	return (in*p[0] + float64(u.OutputTokens)*p[1]) / 1e6
}
