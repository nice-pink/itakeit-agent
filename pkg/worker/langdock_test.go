package worker

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLangdockAsk(t *testing.T) {
	var gotAuth, gotBody string
	sse := func(chunks ...string) string {
		return "data: " + strings.Join(chunks, "\n\ndata: ") + "\n\ndata: [DONE]\n\n"
	}
	reply := sse(`{"choices":[{"delta":{"role":"assistant","content":""}}]}`, `{"choices":[{"delta":{"content":"{\"email\":"}}]}`, `{"choices":[{"delta":{"content":"\"a@b.c\"}"}}]}`, `{"choices":[{"delta":{},"finish_reason":"stop"}]}`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotAuth, gotBody = r.Header.Get("Authorization"), string(b)
		io.WriteString(w, reply)
	}))
	defer srv.Close()
	var dest struct {
		Email string `json:"email"`
	}
	ask := chatAsk("langdock", srv.URL, "m1", "k1")
	if err := ask(context.Background(), "sys", "usr", "high", 100, &dest); err != nil || dest.Email != "a@b.c" {
		t.Fatalf("got %+v, %v", dest, err)
	}
	var req struct {
		Model          string `json:"model"`
		Stream         bool   `json:"stream"`
		ResponseFormat struct {
			Type       string `json:"type"`
			JSONSchema struct {
				Strict bool `json:"strict"`
			} `json:"json_schema"`
		} `json:"response_format"`
	}
	if err := json.Unmarshal([]byte(gotBody), &req); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer k1" || !req.Stream || req.Model != "m1" || req.ResponseFormat.Type != "json_schema" || !req.ResponseFormat.JSONSchema.Strict {
		t.Fatalf("request: %s %s", gotAuth, gotBody)
	}
	for body, want := range map[string]string{
		sse(`{"choices":[{"delta":{"refusal":"no"},"finish_reason":"stop"}]}`):   "declined",
		sse(`{"choices":[{"delta":{"content":"{"},"finish_reason":"length"}]}`):  "cut off",
		sse(`{"choices":[{"delta":{"content":"nope"},"finish_reason":"stop"}]}`): "parse answer",
		sse(`{"choices":[{"delta":{"content":"{}"}}]}`):                          "before the answer was complete",
		sse(`{"error":{"message":"overloaded"}}`):                                "langdock: {\"message\":\"overloaded\"}",
	} {
		reply = body
		if err := ask(context.Background(), "s", "u", "low", 1, &dest); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: got %v, want %q", body, err, want)
		}
	}
}

func TestOpenAIBaseURLAndThink(t *testing.T) {
	var gotPath string
	var sawAuth bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, sawAuth = r.Header["Authorization"]
		io.WriteString(w, `data: {"choices":[{"delta":{"content":"<think>\nmaybe {\"x\":0}\n</think>\n"}}]}`+"\n\n"+`data: {"choices":[{"delta":{"content":"{\"x\":1}"},"finish_reason":"stop"}]}`+"\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	var dest struct {
		X int `json:"x"`
	}
	c := NewOpenAI(srv.URL+"/v1/", "qwen", "", "skills")
	if err := c.ask(context.Background(), "s", "u", "low", 1, &dest); err != nil || dest.X != 1 {
		t.Fatalf("got %+v, %v", dest, err)
	}
	if gotPath != "/v1/chat/completions" || sawAuth {
		t.Fatalf("path %q, auth sent %v", gotPath, sawAuth)
	}
}
