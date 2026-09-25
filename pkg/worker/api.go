package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/shared/constant"
)

// NewAPI calls the Messages API. Credentials come from the environment the way
// the SDK resolves them: ANTHROPIC_API_KEY, ANTHROPIC_AUTH_TOKEN, or an
// `ant auth login` profile.
func NewAPI(model, skills string) *Claude {
	return newAPI(anthropic.NewClient(), model, skills)
}

func newAPI(client anthropic.Client, model, skills string) *Claude {
	return &Claude{ask: apiAsk(client, model), skills: skills}
}

// apiAsk streams each request, so the long max_tokens thinking needs does not
// hit HTTP timeouts.
func apiAsk(client anthropic.Client, model string) askFunc {
	return func(ctx context.Context, system, user, effort string, maxTokens int64, dest any) error {
		stream := client.Beta.Messages.NewStreaming(ctx, anthropic.BetaMessageNewParams{
			Model:        anthropic.Model(model),
			MaxTokens:    maxTokens,
			Betas:        []anthropic.AnthropicBeta{anthropic.AnthropicBetaServerSideFallback2026_07_01},
			Fallbacks:    anthropic.BetaFallbacksParamUnion{OfDefault: constant.ValueOf[constant.Default]()},
			Thinking:     anthropic.BetaThinkingConfigParamUnion{OfAdaptive: &anthropic.BetaThinkingConfigAdaptiveParam{}},
			OutputConfig: anthropic.BetaOutputConfigParam{Effort: anthropic.BetaOutputConfigEffort(effort), Format: anthropic.BetaJSONOutputFormatParam{Schema: dest}},
			// Cached: the system prompt, with the knowledge files, is the same for every task.
			System:   []anthropic.BetaTextBlockParam{{Text: system, CacheControl: anthropic.NewBetaCacheControlEphemeralParam()}},
			Messages: []anthropic.BetaMessageParam{anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(user))},
		})
		defer stream.Close()
		msg := anthropic.BetaMessage{}
		for stream.Next() {
			if err := msg.Accumulate(stream.Current()); err != nil {
				return err
			}
		}
		if err := stream.Err(); err != nil {
			return err
		}
		switch msg.StopReason {
		case anthropic.BetaStopReasonRefusal:
			return errRefused
		case anthropic.BetaStopReasonMaxTokens:
			return fmt.Errorf("answer cut off at %d tokens", maxTokens)
		}
		if err := json.Unmarshal([]byte(answer(msg.Content)), dest); err != nil {
			return fmt.Errorf("parse answer: %w", err)
		}
		return nil
	}
}

// answer is the text after the last fallback block. When a model declines and a
// fallback model takes over, the declined model's partial text comes before it.
func answer(content []anthropic.BetaContentBlockUnion) string {
	var b strings.Builder
	for _, block := range content {
		switch block.Type {
		case "fallback":
			b.Reset()
		case "text":
			b.WriteString(block.Text)
		}
	}
	return b.String()
}
