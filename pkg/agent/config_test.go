package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nice-pink/itakeit/pkg/config"
)

// The example must load in both apps: itakeit ignores the agent block.
func TestExampleConfig(t *testing.T) {
	raw, err := os.ReadFile("../../config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := config.Parse(raw); err != nil {
		t.Fatalf("itakeit rejects it: %v", err)
	}
	c, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if c.Agent.ItakeitUser == "" || c.ClaimDelay().Seconds() != 120 || c.Agent.MaxParallel != 2 {
		t.Fatalf("agent = %+v", c.Agent)
	}
}

func TestConfigRequiresAgentFields(t *testing.T) {
	for _, raw := range []string{"channel: C1\n", "channel: C1\nagent:\n  itakeit_user: U1\n"} {
		if _, err := Parse([]byte(raw)); err == nil || !strings.Contains(err.Error(), "agent.") {
			t.Errorf("%q: err = %v", raw, err)
		}
	}
}

func TestConfigRequiresStatusEmoji(t *testing.T) {
	raw := "channel: C1\nemoji:\n  claim: [raising_hand]\n  done: [white_check_mark]\nagent:\n  itakeit_user: U1\n  skills: x\n"
	if _, err := Parse([]byte(raw)); err == nil || !strings.Contains(err.Error(), "in_progress") {
		t.Fatalf("err = %v", err)
	}
}

func TestConfigBackendDefaults(t *testing.T) {
	base := "channel: C1\nagent:\n  itakeit_user: U1\n  skills: x\n"
	c, err := Parse([]byte(base))
	if err != nil || c.Agent.Backend != BackendClaudeCode || c.Agent.Model != "opus" || c.Agent.ClaudeBin != "claude" {
		t.Fatalf("default: %+v, %v", c.Agent, err)
	}
	c, err = Parse([]byte(base + "  backend: api\n"))
	if err != nil || c.Agent.Model != "claude-opus-5" {
		t.Fatalf("api: %+v, %v", c.Agent, err)
	}
	if _, err := Parse([]byte(base + "  backend: gpt\n")); err == nil {
		t.Fatal("accepted an unknown backend")
	}
}

func TestKnowledgeFiles(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("infra.md", "  Prod namespace is poma-prod.\n")
	write("big.md", strings.Repeat("x", maxKnowledge+1))
	cfg := func(files string) string {
		p := filepath.Join(dir, "config.yaml")
		write("config.yaml", "channel: C1\nagent:\n  itakeit_user: U1\n  skills: x\n  knowledge: ["+files+"]\n")
		return p
	}

	c, err := Load(cfg("infra.md"))
	if err != nil || c.Agent.KnowledgeText != "===== infra.md =====\nProd namespace is poma-prod." {
		t.Fatalf("text = %q, err = %v", c.Agent.KnowledgeText, err)
	}
	if _, err := Load(cfg("missing.md")); err == nil || !strings.Contains(err.Error(), "agent.knowledge") {
		t.Fatalf("missing file: err = %v", err)
	}
	if _, err := Load(cfg("infra.md, big.md")); err == nil || !strings.Contains(err.Error(), "with big.md") {
		t.Fatalf("oversize: err = %v", err)
	}
	// Each file fits, together they do not.
	write("half1.md", strings.Repeat("a", maxKnowledge/2+10))
	write("half2.md", strings.Repeat("b", maxKnowledge/2+10))
	if _, err := Load(cfg("half1.md, half2.md")); err == nil || !strings.Contains(err.Error(), "with half2.md") {
		t.Fatalf("total over cap: err = %v", err)
	}
	// Files with the same base name stay distinguishable.
	os.MkdirAll(filepath.Join(dir, "a"), 0o700)
	os.MkdirAll(filepath.Join(dir, "b"), 0o700)
	write("a/runbook.md", "one")
	write("b/runbook.md", "two")
	c, err = Load(cfg("a/runbook.md, b/runbook.md"))
	if err != nil || !strings.Contains(c.Agent.KnowledgeText, "===== a/runbook.md =====\none") || !strings.Contains(c.Agent.KnowledgeText, "===== b/runbook.md =====\ntwo") {
		t.Fatalf("text = %q, err = %v", c.Agent.KnowledgeText, err)
	}
}
