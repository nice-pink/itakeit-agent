package worker

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
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
	return &Claude{ask: langdockAsk(fmt.Sprintf("https://api.langdock.com/openai/%s/v1/chat/completions", region), model, apiKey), skills: skills}
}

func langdockAsk(url, model, apiKey string) askFunc {
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
		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
			return fmt.Errorf("langdock: %s: %s", resp.Status, bytes.TrimSpace(raw[:min(len(raw), 300)]))
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
				return fmt.Errorf("langdock: unexpected stream data: %w", err)
			}
			if len(chunk.Error) > 0 && string(chunk.Error) != "null" {
				return fmt.Errorf("langdock: %s", chunk.Error)
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
			return errors.New("langdock: stream ended before the answer was complete")
		}
		if err := json.Unmarshal([]byte(content.String()), dest); err != nil {
			return fmt.Errorf("parse answer: %w", err)
		}
		return nil
	}
}
