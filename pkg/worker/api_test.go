package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/nice-pink/itakeit/pkg/task"
)

// serve streams every response as SSE: one content block per entry of blocks
// (a text, or "<fallback>" for a fallback block), then the stop reason. It keeps
// the last request body.
func serve(t *testing.T, stop string, blocks ...string) (*Claude, *map[string]any) {
	t.Helper()
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		send := func(typ string, data map[string]any) {
			data["type"] = typ
			b, _ := json.Marshal(data)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", typ, b)
		}
		send("message_start", map[string]any{"message": map[string]any{"id": "msg_1", "type": "message", "role": "assistant",
			"model": "claude-opus-5", "content": []any{}, "usage": map[string]any{"input_tokens": 1, "output_tokens": 0}}})
		for i, text := range blocks {
			if text == "<fallback>" {
				send("content_block_start", map[string]any{"index": i, "content_block": map[string]any{"type": "fallback"}})
			} else {
				send("content_block_start", map[string]any{"index": i, "content_block": map[string]any{"type": "text", "text": ""}})
				send("content_block_delta", map[string]any{"index": i, "delta": map[string]any{"type": "text_delta", "text": text}})
			}
			send("content_block_stop", map[string]any{"index": i})
		}
		send("message_delta", map[string]any{"delta": map[string]any{"stop_reason": stop}, "usage": map[string]any{"output_tokens": 1}})
		send("message_stop", map[string]any{})
	}))
	t.Cleanup(srv.Close)
	c := newAPI(anthropic.NewClient(option.WithBaseURL(srv.URL), option.WithAPIKey("test"), option.WithMaxRetries(0)), "claude-opus-5", "answering questions")
	return c, &body
}

func TestWorkRequestAndResult(t *testing.T) {
	c, body := serve(t, "end_turn", `{"status":"needs_info","reply":" Which env? "}`)
	res, err := c.Work(context.Background(), "Task reported by <@U1>:\nfix it")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != task.NeedsInfo || res.Reply != "Which env?" {
		t.Fatalf("result = %+v", res)
	}
	b := *body
	if b["fallbacks"] != "default" || b["thinking"].(map[string]any)["type"] != "adaptive" {
		t.Fatalf("fallbacks/thinking = %v %v", b["fallbacks"], b["thinking"])
	}
	oc := b["output_config"].(map[string]any)
	schema, _ := json.Marshal(oc["format"].(map[string]any)["schema"])
	for _, want := range []string{`"additionalProperties":false`, `"needs_info"`, `"required":["status","reply"]`} {
		if !strings.Contains(string(schema), want) {
			t.Errorf("schema %s lacks %s", schema, want)
		}
	}
	if oc["effort"] != "high" {
		t.Errorf("effort = %v", oc["effort"])
	}
}

func TestWorkRefusalBlocks(t *testing.T) {
	c, _ := serve(t, "refusal")
	res, err := c.Work(context.Background(), "x")
	if err != nil || res.Status != task.Blocked {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
}

func TestWorkRejectsUnknownStatus(t *testing.T) {
	c, _ := serve(t, "end_turn", `{"status":"in_progress","reply":"x"}`)
	if _, err := c.Work(context.Background(), "x"); err == nil {
		t.Fatal("accepted a status the schema does not allow")
	}
}

func TestTriage(t *testing.T) {
	c, body := serve(t, "end_turn", `{"take":true,"reason":"I will explain it."}`)
	take, reason, err := c.Triage(context.Background(), "what is x?")
	if err != nil || !take || reason != "I will explain it." {
		t.Fatalf("take = %v, reason = %q, err = %v", take, reason, err)
	}
	if (*body)["output_config"].(map[string]any)["effort"] != "low" {
		t.Error("triage effort is not low")
	}
}

func TestWorkParsesAnswerAfterFallback(t *testing.T) {
	c, _ := serve(t, "end_turn", `{"status":"do`, "<fallback>", `{"status":"done",`, `"reply":"ok"}`)
	res, err := c.Work(context.Background(), "x")
	if err != nil || res.Status != task.Done || res.Reply != "ok" {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
}

func TestWorkMaxTokensFails(t *testing.T) {
	c, _ := serve(t, "max_tokens", `{"status":"done","re`)
	if _, err := c.Work(context.Background(), "x"); err == nil || !strings.Contains(err.Error(), "cut off") {
		t.Fatalf("err = %v", err)
	}
}

func TestAPIProbe(t *testing.T) {
	c, body := serve(t, "end_turn", `{"email":""}`)
	refused, err := c.Probe(context.Background())
	if err != nil || refused || c.Secrets() != 0 {
		t.Fatalf("refused = %v, err = %v, secrets = %d", refused, err, c.Secrets())
	}
	if (*body)["max_tokens"].(float64) != 16000 {
		t.Errorf("max_tokens = %v", (*body)["max_tokens"])
	}
	c, _ = serve(t, "max_tokens", `{"email":"`)
	if _, err := c.Probe(context.Background()); err == nil || !strings.Contains(err.Error(), "startup probe") {
		t.Fatalf("cut-off probe: err = %v", err)
	}
}

func TestAPIKnowledgeInCachedSystem(t *testing.T) {
	c, body := serve(t, "end_turn", `{"status":"done","reply":"ok"}`)
	c.Knowledge = "===== infra.md =====\nProd is poma-prod."
	if _, err := c.Work(context.Background(), "which namespace?"); err != nil {
		t.Fatal(err)
	}
	sys := (*body)["system"].([]any)[0].(map[string]any)
	if !strings.Contains(sys["text"].(string), "Prod is poma-prod.") || sys["cache_control"].(map[string]any)["type"] != "ephemeral" {
		t.Fatalf("system = %v", sys)
	}
	msg, _ := json.Marshal((*body)["messages"])
	if strings.Contains(string(msg), "poma-prod") {
		t.Fatalf("knowledge in the user message: %s", msg)
	}
}
