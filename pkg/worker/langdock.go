package worker

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// NewLangdock calls Langdock's OpenAI-compatible chat completions endpoint
// (https://api.langdock.com/openai/{region}/v1/chat/completions) with the
// schema of dest as a strict json_schema response format. Any model of the
// workspace works. Each request streams, so a long answer does not meet a
// gateway timeout for non-streaming calls. Effort and max tokens are not sent:
// not every model behind the endpoint accepts them.
func NewLangdock(region, model, apiKey, skills string) *Claude {
	return &Claude{ask: chatAsk("langdock", fmt.Sprintf("https://api.langdock.com/openai/%s/v1/chat/completions", region), model, apiKey), skills: skills}
}

// NewOpenAI calls the chat completions endpoint under baseURL the same way
// (OpenAI by default, or any compatible server such as vLLM, SGLang or Ollama).
// Models that reject strict json_schema output fail the startup probe. An empty
// apiKey sends no Authorization header, for servers without auth.
func NewOpenAI(baseURL, model, apiKey, skills string) *Claude {
	return &Claude{ask: chatAsk("openai", strings.TrimRight(baseURL, "/")+"/chat/completions", model, apiKey), skills: skills}
}

// chatAsk speaks the OpenAI chat completions protocol, which Langdock mirrors.
// provider only names the backend in errors.
func chatAsk(provider, url, model, apiKey string) askFunc {
	client := &http.Client{Timeout: 15 * time.Minute}
	return func(ctx context.Context, system, user, _ string, _ int64, dest any) error {
		schema, err := schemaOf(dest)
		if err != nil {
			return err
		}
		body, err := json.Marshal(map[string]any{
			"model":  model,
			"stream": true,
			"messages": []map[string]string{
				{"role": "system", "content": system},
				{"role": "user", "content": user},
			},
			"response_format": map[string]any{
				"type":        "json_schema",
				"json_schema": map[string]any{"name": "answer", "strict": true, "schema": json.RawMessage(schema)},
			},
		})
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return err
		}
		if apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+apiKey)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
			return fmt.Errorf("%s: %s: %s", provider, resp.Status, bytes.TrimSpace(raw[:min(len(raw), 300)]))
		}
		var content strings.Builder
		var refusal, finish string
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 64<<10), 4<<20)
		for sc.Scan() {
			data, ok := strings.CutPrefix(sc.Text(), "data:")
			if data = strings.TrimSpace(data); !ok || data == "" {
				continue
			}
			if data == "[DONE]" {
				break
			}
			var chunk struct {
				Error   json.RawMessage `json:"error"`
				Choices []struct {
					FinishReason string `json:"finish_reason"`
					Delta        struct {
						Content string `json:"content"`
						Refusal string `json:"refusal"`
					} `json:"delta"`
				} `json:"choices"`
			}
			if err := json.Unmarshal([]byte(data), &chunk); err != nil {
				return fmt.Errorf("%s: unexpected stream data: %w", provider, err)
			}
			if len(chunk.Error) > 0 && string(chunk.Error) != "null" {
				return fmt.Errorf("%s: %s", provider, chunk.Error)
			}
			for _, c := range chunk.Choices {
				content.WriteString(c.Delta.Content)
				refusal += c.Delta.Refusal
				finish = cmp.Or(c.FinishReason, finish)
			}
		}
		if err := sc.Err(); err != nil {
			return err
		}
		switch {
		case refusal != "", finish == "content_filter":
			return errRefused
		case finish == "length":
			return fmt.Errorf("answer cut off")
		case finish == "":
			return fmt.Errorf("%s: stream ended before the answer was complete", provider)
		}
		if err := json.Unmarshal([]byte(stripThink(content.String())), dest); err != nil {
			return fmt.Errorf("parse answer: %w", err)
		}
		return nil
	}
}

// stripThink drops the leading <think>…</think> block that Qwen and other
// reasoning models write into the content when the server runs no reasoning
// parser (vLLM needs --reasoning-parser for that).
func stripThink(s string) string {
	t := strings.TrimSpace(s)
	if rest, ok := strings.CutPrefix(t, "<think>"); ok {
		if _, after, found := strings.Cut(rest, "</think>"); found {
			return strings.TrimSpace(after)
		}
	}
	return s
}
