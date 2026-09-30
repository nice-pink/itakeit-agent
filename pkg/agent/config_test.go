package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nice-pink/itakeit-agent/pkg/worker"
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
	if c.Agent.ItakeitApp == "" || c.ClaimDelay().Seconds() != 120 || c.Agent.MaxParallel != 2 {
		t.Fatalf("agent = %+v", c.Agent)
	}
}

func TestConfigRequiresAgentFields(t *testing.T) {
	for _, raw := range []string{"channel: C1\n", "channel: C1\nagent:\n  itakeit_user: U11\n"} {
		if _, err := Parse([]byte(raw)); err == nil || !strings.Contains(err.Error(), "agent.") {
			t.Errorf("%q: err = %v", raw, err)
		}
	}
}

func TestConfigRequiresStatusEmoji(t *testing.T) {
	raw := "channel: C1\nemoji:\n  claim: [raising_hand]\n  done: [white_check_mark]\nagent:\n  itakeit_user: U11\n  skills: x\n"
	if _, err := Parse([]byte(raw)); err == nil || !strings.Contains(err.Error(), "in_progress") {
		t.Fatalf("err = %v", err)
	}
}

func TestConfigBackendDefaults(t *testing.T) {
	base := "channel: C1\nagent:\n  itakeit_user: U11\n  skills: x\n"
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
		write("config.yaml", "channel: C1\nagent:\n  itakeit_user: U11\n  skills: x\n  knowledge: ["+files+"]\n")
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

func TestConfigEnv(t *testing.T) {
	base := "channel: C1\nagent:\n  itakeit_user: U11\n  skills: x\n  env: "
	if c, err := Parse([]byte(base + "[KUBECONFIG, https_proxy]\n")); err != nil || len(c.Agent.Env) != 2 {
		t.Fatalf("valid: %+v, %v", c, err)
	}
	for _, bad := range []string{"[AGENT_SLACK_BOT_TOKEN]", "[agent_slack_app_token]", "[ANTHROPIC_API_KEY]", "[anthropic_api_key]", "[ANTHROPIC_AUTH_TOKEN]", "[\"A=B\"]", "[\"1X\"]"} {
		if _, err := Parse([]byte(base + bad + "\n")); err == nil || !strings.Contains(err.Error(), "agent.env") {
			t.Errorf("%s: err = %v", bad, err)
		}
	}
}

func TestConfigTools(t *testing.T) {
	base := "channel: C1\nagent:\n  itakeit_user: U11\n  skills: x\n"
	c, err := Parse([]byte(base))
	if err != nil || c.Agent.WorkerTools() != nil || c.Agent.Mode != "propose" {
		t.Fatalf("no tools: %+v, %v", c.Agent, err)
	}
	c, err = Parse([]byte(base + "  tools:\n    read: [Read, \"Bash(kubectl get *)\"]\n  work_timeout_minutes: 20\n"))
	if err != nil {
		t.Fatal(err)
	}
	tools := c.Agent.WorkerTools()
	if tools == nil || tools.Mode != worker.ModePropose || len(tools.Read) != 2 || tools.Read[1].Prefix != "kubectl get" || tools.WorkTimeout != 20*time.Minute {
		t.Fatalf("tools = %+v", tools)
	}
	for bad, want := range map[string]string{
		"  mode: fix\n":                                "needs agent.tools.write",
		"  mode: yolo\n":                               "agent.mode",
		"  tools:\n    read: [Edit]\n":                 "agent.tools.read",
		"  tools:\n    read: [\"Bash(sh *)\"]\n":       "agent.tools.read",
		"  backend: api\n  tools:\n    read: [Read]\n": "backend claude-code",
		"  work_timeout_minutes: 500\n":                "work_timeout_minutes",
	} {
		if _, err := Parse([]byte(base + bad)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want %q", bad, err, want)
		}
	}
}

func TestConfigFixMode(t *testing.T) {
	base := "channel: C1\nemoji:\n  claim: [raising_hand]\n  in_progress: [construction]\n  needs_info: [question]\n  blocked: [no_entry]\n  done: [white_check_mark]\nagent:\n  itakeit_user: UBOT1\n  skills: x\n  max_parallel: 3\n"
	fix := "  mode: fix\n  tools:\n    write: [\"Bash(kubectl rollout restart *)\"]\n"
	c, err := Parse([]byte(base + fix + "  approver: UAPP\n"))
	if err != nil {
		t.Fatal(err)
	}
	s := c.Agent
	tools := s.WorkerTools()
	if tools.Mode != worker.ModeFix || !tools.Approval || len(tools.Write) != 1 || s.ApprovalEmoji.Approve != "heavy_check_mark" || s.ApprovalEmoji.Deny != "x" ||
		s.ApprovalTimeoutMinutes != 30 || s.MaxSessions != 3 {
		t.Fatalf("settings = %+v, tools = %+v", s, tools)
	}
	if _, err := Parse([]byte(base + fix + "  allow_unapproved_writes: true\n")); err != nil {
		t.Fatalf("fix without approver, allowed: %v", err)
	}
	if c, err := Parse([]byte(base + fix + "  approver: UAPP\n  allow_real_home: true\n")); err != nil || !c.Agent.WorkerTools().RealHome {
		t.Fatalf("allow_real_home: err = %v", err)
	}
	// Propose mode never runs write entries, so they do not count against it.
	if _, err := Parse([]byte(base + "  allow_real_home: true\n  approver: UAPP\n  tools:\n    read: [Read]\n    write: [\"Bash(kubectl config *)\"]\n")); err != nil {
		t.Fatalf("allow_real_home in propose mode: %v", err)
	}
	for bad, want := range map[string]string{
		fix:                               "allow_unapproved_writes",
		"  mode: fix\n  approver: UAPP\n": "needs agent.tools.write",
		fix + "  approver: bob\n":         "Slack member ID",
		fix + "  approver: UBOT1\n":       "itakeit bot",
		"  mode: fix\n  approver: UAPP\n  tools:\n    read: [\"Bash(kubectl get *)\"]\n    write: [\"Bash(kubectl get *)\"]\n":                                  "also covers write entry",
		"  mode: fix\n  approver: UAPP\n  tools:\n    read: [\"Bash(kubectl *)\"]\n    write: [\"Bash(kubectl rollout restart *)\"]\n":                          "also covers write entry",
		"  mode: fix\n  approver: UAPP\n  tools:\n    read: [\"Bash(kubectl rollout restart deploy/api)\"]\n    write: [\"Bash(kubectl rollout restart *)\"]\n": "also covers write entry",
		"  mode: fix\n  approver: UAPP\n  tools:\n    read: [\"Bash(kubectl get *)\"]\n    write: [Bash]\n":                                                     "also covers write entry",
		"  mode: fix\n  allow_unapproved_writes: true\n  tools:\n    write: [Bash]\n":                                                                           "needs agent.approver",
		"  mode: fix\n  allow_unapproved_writes: true\n  tools:\n    write: [\"Bash(sed -i *)\"]\n":                                                             "needs agent.approver",
		"  mode: fix\n  approver: UAPP\n  allow_real_home: true\n  tools:\n    write: [Bash]\n":                                                                 "allow_real_home",
		"  mode: fix\n  approver: UAPP\n  allow_real_home: true\n  tools:\n    write: [\"Bash(tee *)\"]\n":                                                      "allow_real_home",
		"  mode: fix\n  approver: UAPP\n  allow_real_home: true\n  tools:\n    write: [\"Bash(kubectl config *)\"]\n":                                           "change files under HOME",
		"  mode: fix\n  approver: UAPP\n  allow_real_home: true\n  tools:\n    write: [\"Bash(kubectl cp *)\"]\n":                                               "change files under HOME",
		"  mode: fix\n  approver: UAPP\n  allow_real_home: true\n  tools:\n    write: [\"Bash(helm repo add *)\"]\n":                                            "change files under HOME",
		"  mode: fix\n  approver: UAPP\n  allow_real_home: true\n  tools:\n    write: [\"Bash(gcloud container clusters get-credentials *)\"]\n":                "change files under HOME",
		"  mode: fix\n  approver: UAPP\n  allow_real_home: true\n  tools:\n    write: [\"Bash(gcloud container *)\"]\n":                                         "change files under HOME",
		"  mode: fix\n  approver: UAPP\n  allow_real_home: true\n  tools:\n    write: [\"Bash(aws eks update-kubeconfig *)\"]\n":                                "change files under HOME",
		"  mode: fix\n  approver: UAPP\n  allow_real_home: true\n  tools:\n    write: [\"Bash(kubectl *)\"]\n":                                                  "change files under HOME",
		"  mode: fix\n  allow_unapproved_writes: true\n  allow_real_home: true\n  tools:\n    write: [\"Bash(kubectl rollout restart *)\"]\n":                   "needs agent.approver",
		fix + "  approver: UAPP\n  approval_emoji:\n    approve: white_check_mark\n":                                                                            "itakeit's done emoji",
		fix + "  approver: UAPP\n  approval_emoji:\n    approve: x\n":                                                                                           "must differ",
		fix + "  approver: UAPP\n  approval_timeout_minutes: 41\n":                                                                                              "between 1 and 40",
		fix + "  approver: UAPP\n  backend: api\n":                                                                                                              "backend claude-code",
		"  approver: UAPP\n  backend: api\n": "backend claude-code",
	} {
		if _, err := Parse([]byte(base + bad)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want %q", bad, err, want)
		}
	}
}

func TestConfigMCP(t *testing.T) {
	base := "channel: C1\nagent:\n  itakeit_user: UBOT1\n  skills: x\n"
	good := "  mcp_servers:\n    gh:\n      type: stdio\n      command: gh-mcp\n      env: [GH_TOKEN]\n    docs:\n      type: http\n      url: https://mcp.example.com\n      headers:\n        Authorization: DOCS_AUTH\n  tools:\n    read: [mcp__gh__get_issue, mcp__docs__search]\n"
	c, err := Parse([]byte(base + good))
	if err != nil {
		t.Fatal(err)
	}
	tools := c.Agent.WorkerTools()
	if len(tools.MCP) != 2 || tools.MCP["gh"].Env[0] != "GH_TOKEN" || tools.MCP["docs"].Headers["Authorization"] != "DOCS_AUTH" || len(tools.Read) != 2 {
		t.Fatalf("tools = %+v", tools)
	}
	for bad, want := range map[string]string{
		"  tools:\n    read: [mcp__gh__get_issue]\n":                                                                      "does not configure",
		"  mcp_servers:\n    g_h:\n      type: stdio\n      command: x\n":                                                 "no underscores",
		"  mcp_servers:\n    gh:\n      type: stdio\n":                                                                    "stdio needs command",
		"  mcp_servers:\n    gh:\n      type: http\n      url: http://plain.example.com\n":                                "https:// url",
		"  mcp_servers:\n    gh:\n      type: sse\n      url: https://x\n":                                                "stdio or http",
		"  mcp_servers:\n    gh:\n      type: stdio\n      command: x\n      env: [AGENT_SLACK_BOT_TOKEN]\n":              "must not pass",
		"  env: [GH_TOKEN]\n  mcp_servers:\n    gh:\n      type: stdio\n      command: x\n      env: [GH_TOKEN]\n":        "also reaches the CLI's environment",
		"  mcp_servers:\n    gh:\n      type: stdio\n      command: x\n      env: [CLAUDE_CODE_OAUTH_TOKEN]\n":            "also reaches the CLI's environment",
		"  mcp_servers:\n    gh:\n      type: stdio\n      command: x\n      env: [HTTPS_PROXY]\n":                        "also reaches the CLI's environment",
		"  mcp_servers:\n    gh:\n      type: stdio\n      command: ./srv\n":                                              "absolute",
		"  mcp_servers:\n    gh:\n      type: http\n      url: https://\n":                                                "with a host",
		"  mcp_servers:\n    gh:\n      type: http\n      url: https://u:hunter2secret@h/mcp\n":                           "no credentials",
		"  mcp_servers:\n    gh:\n      type: http\n      url: https://h\n      headers:\n        \"bad name\": X_AUTH\n": "not a header name",
		"  mcp_servers:\n    gh:\n      type: stdio\n      command: gh-mcp\n":                                             "needs agent.tools entries",
		"  backend: api\n  mcp_servers:\n    gh:\n      type: stdio\n      command: x\n":                                  "backend claude-code",
	} {
		if _, err := Parse([]byte(base + bad)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want %q", bad, err, want)
		}
	}
}

func TestConfigMemory(t *testing.T) {
	base := "channel: C1\nagent:\n  itakeit_user: U11\n  skills: x\n"
	c, err := Parse([]byte(base))
	if err != nil || c.Agent.Memory.Enabled || c.Agent.Memory.NeedsApproval() {
		t.Fatalf("off by default: %+v, %v", c.Agent.Memory, err)
	}
	c, err = Parse([]byte(base + "  approver: U22\n  memory:\n    enabled: true\n"))
	if m := c.Agent.Memory; err != nil || m.Dir != "memory" || m.Bin != "poma-memory" || m.Results != 3 || !m.NeedsApproval() {
		t.Fatalf("defaults: %+v, %v", m, err)
	}
	c, err = Parse([]byte(base + "  memory:\n    enabled: true\n    approval: false\n"))
	if err != nil || c.Agent.Memory.NeedsApproval() {
		t.Fatalf("approval off: %+v, %v", c.Agent.Memory, err)
	}
	// The API backend takes an approver only for learnings.
	if _, err := Parse([]byte(base + "  backend: api\n  approver: U22\n  memory:\n    enabled: true\n")); err != nil {
		t.Fatalf("api with memory approval: %v", err)
	}
	for name, raw := range map[string]string{
		"approval needs approver":       "  memory:\n    enabled: true\n",
		"results range":                 "  approver: U22\n  memory:\n    enabled: true\n    results: 11\n",
		"relative bin":                  "  approver: U22\n  memory:\n    enabled: true\n    bin: ./poma-memory\n",
		"api approver without learning": "  backend: api\n  approver: U22\n  memory:\n    enabled: true\n    approval: false\n",
	} {
		if _, err := Parse([]byte(base + raw)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestLoadMemoryDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("channel: C1\nagent:\n  itakeit_user: U11\n  skills: x\n  memory:\n    enabled: true\n    approval: false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil || c.Agent.Memory.Dir != filepath.Join(dir, "memory") {
		t.Fatalf("dir = %q, %v", c.Agent.Memory.Dir, err)
	}
}

func TestConfigItakeitIdentity(t *testing.T) {
	base := "channel: C1\nagent:\n  skills: x\n"
	c, err := Parse([]byte(base + "  itakeit_app: A0ITAKEIT\n"))
	if err != nil || c.Agent.ItakeitApp != "A0ITAKEIT" {
		t.Fatalf("app id alone: %+v, %v", c, err)
	}
	for name, raw := range map[string]string{
		"neither":        "",
		"app id as user": "  itakeit_user: A0ITAKEIT\n",
		"user id as app": "  itakeit_app: U0ITAKEIT\n",
		"bot id as app":  "  itakeit_app: B0ITAKEIT\n",
	} {
		if _, err := Parse([]byte(base + raw)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
