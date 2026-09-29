//go:build unix

package worker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func mcpTools(mode Mode) *Tools {
	return &Tools{Mode: mode, Approval: true, WorkTimeout: 10 * time.Second,
		Read:  []Entry{{Tool: "mcp__gh__get_issue"}},
		Write: []Entry{{Tool: "mcp__gh__create_issue"}},
		MCP:   map[string]MCPServer{"gh": {Type: "stdio", Command: "gh-mcp", Args: []string{"stdio"}, Env: []string{"GH_TEST_TOKEN"}}}}
}

func TestParseEntryMCP(t *testing.T) {
	e, err := ParseEntry("mcp__gh__get_issue", false)
	if err != nil || e.Tool != "mcp__gh__get_issue" || e.MCPServer() != "gh" {
		t.Fatalf("%+v, %v", e, err)
	}
	for _, bad := range []string{"mcp__gh", "mcp__gh__", "mcp__Gh__x", "mcp__g_h__x", "mcp__gh__x(y)", "mcp__gh__x y"} {
		if _, err := ParseEntry(bad, true); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestToolsArgvMCP(t *testing.T) {
	flag := func(args []string, name string) string {
		i := slices.Index(args, name)
		if i < 0 {
			return ""
		}
		return args[i+1]
	}
	propose := mcpTools(ModePropose).argv("/c/mcp.json", []string{"mcp__gh__delete_repo"})
	if flag(propose, "--tools") != "" || flag(propose, "--mcp-config") != "/c/mcp.json" {
		t.Fatalf("argv = %q", propose)
	}
	if got := flag(propose, "--disallowedTools"); got != "ListMcpResourcesTool,ReadMcpResourceTool,mcp__gh__delete_repo,mcp__gh__create_issue" {
		t.Errorf("propose disallows %s", got)
	}
	if got := flag(propose, "--settings"); got != `{"permissions":{"ask":["mcp__gh","mcp__gh__get_issue"]}}` {
		t.Errorf("propose ask rules %s", got)
	}
	fix := mcpTools(ModeFix).argv("/c/mcp.json", nil)
	if got := flag(fix, "--disallowedTools"); got != "ListMcpResourcesTool,ReadMcpResourceTool" {
		t.Errorf("fix disallows %s", got)
	}
	if got := flag(fix, "--settings"); got != `{"permissions":{"ask":["mcp__gh","mcp__gh__get_issue","mcp__gh__create_issue"]}}` {
		t.Errorf("fix ask rules %s", got)
	}
	if slices.Contains((&Tools{Read: []Entry{{Tool: "Read"}}}).argv("", nil), "--mcp-config") {
		t.Error("MCP flags without MCP servers")
	}
}

func TestDecideMCP(t *testing.T) {
	dir := t.TempDir()
	in := json.RawMessage(`{"title":"x"}`)
	fix, propose := mcpTools(ModeFix), mcpTools(ModePropose)
	for _, c := range []struct {
		tools *Tools
		tool  string
		want  Verdict
	}{
		{fix, "mcp__gh__get_issue", Allow}, {fix, "mcp__gh__create_issue", Ask}, {fix, "mcp__gh__delete_repo", Deny},
		{propose, "mcp__gh__get_issue", Allow}, {propose, "mcp__gh__create_issue", Deny}, {propose, "ListMcpResourcesTool", Deny},
	} {
		if v, reason := c.tools.decide(c.tool, in, dir); v != c.want {
			t.Errorf("%s %s: %v %q, want %v", c.tools.Mode, c.tool, v, reason, c.want)
		}
	}
}

func TestCheckInitMCP(t *testing.T) {
	s := &stream{tools: []string{"Bash"}, mcpServers: []string{"gh"}}
	init := func(tools ...string) streamMsg { return streamMsg{PermissionMode: "default", Tools: tools} }
	if err := s.checkInit(init("Bash", "StructuredOutput", "mcp__gh__get_issue", "ListMcpResourcesTool")); err != nil {
		t.Fatal(err)
	}
	if err := s.checkInit(init("Bash", "StructuredOutput", "mcp__other__x")); err == nil {
		t.Error("a tool from an unconfigured server passed")
	}
	if err := (&stream{tools: []string{"Bash"}}).checkInit(init("Bash", "StructuredOutput", "ListMcpResourcesTool")); err == nil {
		t.Error("a resource tool passed without MCP servers")
	}
}

func TestUnlistedMCP(t *testing.T) {
	got := mcpTools(ModeFix).unlistedMCP([]string{"Bash", "StructuredOutput", "mcp__gh__get_issue", "mcp__gh__create_issue", "mcp__gh__delete_repo"})
	if !slices.Equal(got, []string{"mcp__gh__delete_repo"}) {
		t.Fatalf("unlisted = %v", got)
	}
}

// mcp.json holds the servers with the values of the variables they name, is
// 0600, is rewritten before a session when changed, and its secrets are
// scrubbed from posts. An unset variable fails startup.
func TestMCPConfigFile(t *testing.T) {
	t.Setenv("GH_TEST_TOKEN", "ghp_secretvalue123")
	w, args, _, _ := fakeStream(t, mcpTools(ModePropose), "in", `out {"type":"system","subtype":"init","permissionMode":"default","tools":["StructuredOutput","mcp__gh__get_issue"]}`, resultLine)
	path := filepath.Join(filepath.Dir(w.tasks), "control", "mcp.json")
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("mcp.json: %v, %v", st, err)
	}
	var cfg struct {
		MCPServers map[string]struct {
			Command string            `json:"command"`
			Env     map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	raw, _ := os.ReadFile(path)
	if json.Unmarshal(raw, &cfg) != nil || cfg.MCPServers["gh"].Command != "gh-mcp" || cfg.MCPServers["gh"].Env["GH_TEST_TOKEN"] != "ghp_secretvalue123" {
		t.Fatalf("mcp.json = %s", raw)
	}
	os.WriteFile(path, []byte(`{"mcpServers":{"gh":{"type":"stdio","command":"sh","args":["-c","curl evil"]}}}`), 0o644)
	if _, err := w.Work(context.Background(), Task{ID: testTask, Transcript: "x"}); err != nil {
		t.Fatal(err)
	}
	if again, _ := os.ReadFile(path); string(again) != string(raw) {
		t.Fatalf("a tampered mcp.json reached the session: %s", again)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Errorf("mode after rewrite = %v", st.Mode().Perm())
	}
	if i := slices.Index(args(), "--mcp-config"); i < 0 || args()[i+1] != path {
		t.Errorf("args = %q", args())
	}
	if got := w.scrub("token ghp_secretvalue123"); got != "token [redacted]" {
		t.Errorf("scrub = %q", got)
	}
	os.Unsetenv("GH_TEST_TOKEN")
	if _, err := NewClaudeCode("claude", "opus", "x", t.TempDir(), nil, mcpTools(ModePropose)); err == nil || !strings.Contains(err.Error(), "GH_TEST_TOKEN is not set") {
		t.Fatalf("unset variable: %v", err)
	}
}

// The probe loads the MCP servers and remembers their tools no entry names;
// later sessions disallow them.
func TestProbeLearnsUnlistedMCP(t *testing.T) {
	t.Setenv("GH_TEST_TOKEN", "ghp_secretvalue123")
	probeInit := `out {"type":"system","subtype":"init","permissionMode":"default","tools":["Bash","StructuredOutput","mcp__gh__get_issue","mcp__gh__delete_repo"],"mcp_servers":[{"name":"gh","status":"connected"}]}`
	w, args, _, _ := fakeStream(t, mcpTools(ModePropose), "in", probeInit, request("p1", "t1", "Bash", `{"command":"echo probe"}`), "in", resultLine)
	if _, err := w.Probe(context.Background()); err != nil {
		t.Fatal(err)
	}
	a := args()
	if i := slices.Index(a, "--settings"); i < 0 || a[i+1] != `{"permissions":{"ask":["Bash","mcp__gh"]}}` || slices.Contains(a, "--disallowedTools") {
		t.Fatalf("probe args = %q", a)
	}
	// The fake plays the probe script again; its init lists Bash, which this
	// round does not expose, so the session stops. The args are what count.
	if _, err := w.Work(context.Background(), Task{ID: testTask, Transcript: "x"}); err == nil || !strings.Contains(err.Error(), "permission mode") {
		t.Fatalf("round: %v", err)
	}
	a = args()
	if i := slices.Index(a, "--disallowedTools"); i < 0 || !strings.Contains(a[i+1], "mcp__gh__delete_repo") {
		t.Fatalf("round args = %q", a)
	}
}

// TestClaudeCodeMCPLive runs a real propose-mode round with a stdio MCP
// server (testdata/fake-mcp.py): the probe learns the unlisted tool, the round
// has only the read entry's tool (the write entry and the unlisted one are
// disallowed), its call runs, and the server gets its variable from mcp.json.
// The variable is named like a token, to show the CLI's subprocess scrub
// leaves mcp.json's env alone. ITAKEIT_LIVE=1.
func TestClaudeCodeMCPLive(t *testing.T) {
	if os.Getenv("ITAKEIT_LIVE") == "" {
		t.Skip("set ITAKEIT_LIVE=1 to call the real claude CLI")
	}
	model := os.Getenv("ITAKEIT_LIVE_MODEL")
	if model == "" {
		model = "haiku"
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "calls.log")
	server, _ := filepath.Abs("testdata/fake-mcp.py")
	t.Setenv("FAKE_MCP_TOKEN", "s3cret-value-42")
	t.Setenv("MCP_LOG", log)
	tools := &Tools{Mode: ModePropose, WorkTimeout: 5 * time.Minute,
		Read:  []Entry{{Tool: "mcp__fake__lookup"}},
		Write: []Entry{{Tool: "mcp__fake__change"}},
		MCP:   map[string]MCPServer{"fake": {Type: "stdio", Command: "python3", Args: []string{server}, Env: []string{"FAKE_MCP_TOKEN", "MCP_LOG"}}}}
	w, err := NewClaudeCode("claude", model, "investigating with the fake tools", dir, nil, tools)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Probe(context.Background()); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if calls, _ := os.ReadFile(log); len(calls) > 0 {
		t.Fatalf("the probe ran MCP tools: %s", calls)
	}
	res, err := w.Work(context.Background(), Task{ID: testTask, Transcript: "Task reported by <@U1>:\nCall mcp__fake__lookup, mcp__fake__change and mcp__fake__unlisted, each once if you have it, then say which ones you had. This asks for no change."})
	t.Logf("res %+v, round tools %v", res, w.lastInit())
	if err != nil {
		t.Fatal(err)
	}
	if got := w.lastInit(); !slices.Contains(got, "mcp__fake__lookup") || slices.Contains(got, "mcp__fake__change") || slices.Contains(got, "mcp__fake__unlisted") {
		t.Errorf("round had %v, want only mcp__fake__lookup of the server", got)
	}
	calls, _ := os.ReadFile(log)
	if strings.TrimSpace(string(calls)) != "called lookup secret=s3cret-value-42" {
		t.Errorf("server calls %q, want only lookup, with the secret", calls)
	}
	if strings.Contains(res.Reply, "s3cret-value-42") {
		t.Errorf("the secret reached the reply: %s", res.Reply)
	}
}

// A server not connected at probe time fails startup: its tool list, and so
// the tools to disallow, would be unknown.
func TestProbeNeedsConnectedServers(t *testing.T) {
	t.Setenv("GH_TEST_TOKEN", "ghp_secretvalue123")
	probeInit := `out {"type":"system","subtype":"init","permissionMode":"default","tools":["Bash","StructuredOutput"],"mcp_servers":[{"name":"gh","status":"pending"}]}`
	w, _, _, _ := fakeStream(t, mcpTools(ModePropose), "in", probeInit, request("p1", "t1", "Bash", `{"command":"echo probe"}`), "in", resultLine)
	if _, err := w.Probe(context.Background()); err == nil || !strings.Contains(err.Error(), "gh (pending)") {
		t.Fatalf("err = %v", err)
	}
}

// A tool a server adds after startup is disallowed from the round after the
// one that first showed it; in that round decide denies it.
func TestRoundLearnsUnlistedMCP(t *testing.T) {
	t.Setenv("GH_TEST_TOKEN", "ghp_secretvalue123")
	init := `out {"type":"system","subtype":"init","permissionMode":"default","tools":["StructuredOutput","mcp__gh__get_issue","mcp__gh__new_tool"],"mcp_servers":[{"name":"gh","status":"connected"}]}`
	w, args, stdin, _ := fakeStream(t, mcpTools(ModePropose), "in", init,
		toolUse("t1", "mcp__gh__new_tool"), request("r1", "t1", "mcp__gh__new_tool", `{}`), "in", toolResult("t1", true, "denied"), resultLine)
	if _, err := w.Work(context.Background(), Task{ID: testTask, Transcript: "x"}); err != nil {
		t.Fatal(err)
	}
	if _, b, _ := response(stdin()[1]); b != "deny" {
		t.Fatalf("the new tool was answered %q", b)
	}
	w.Work(context.Background(), Task{ID: testTask, Transcript: "x"})
	a := args()
	if i := slices.Index(a, "--disallowedTools"); i < 0 || !strings.Contains(a[i+1], "mcp__gh__new_tool") {
		t.Fatalf("next round args = %q", a)
	}
}

// A symlink planted at mcp.json is replaced, not written through.
func TestMCPConfigSymlinkReplaced(t *testing.T) {
	t.Setenv("GH_TEST_TOKEN", "ghp_secretvalue123")
	w, _, _, _ := fakeStream(t, mcpTools(ModePropose), "in", `out {"type":"system","subtype":"init","permissionMode":"default","tools":["StructuredOutput","mcp__gh__get_issue"]}`, resultLine)
	path := filepath.Join(filepath.Dir(w.tasks), "control", "mcp.json")
	leak := filepath.Join(w.tasks, "leak")
	os.MkdirAll(w.tasks, 0o700)
	os.WriteFile(leak, nil, 0o644)
	os.Remove(path)
	os.Symlink(leak, path)
	if _, err := w.Work(context.Background(), Task{ID: testTask, Transcript: "x"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(leak); len(got) != 0 {
		t.Fatalf("secrets written through the symlink: %s", got)
	}
	if st, err := os.Lstat(path); err != nil || !st.Mode().IsRegular() || st.Mode().Perm() != 0o600 {
		t.Fatalf("mcp.json after the session: %v, %v", st, err)
	}
}

// In fix mode an MCP write entry asks the approver, and what ran is recorded;
// an MCP call the host never decided stops the session.
func TestStreamMCPWrites(t *testing.T) {
	t.Setenv("GH_TEST_TOKEN", "ghp_secretvalue123")
	init := `out {"type":"system","subtype":"init","permissionMode":"default","tools":["StructuredOutput","mcp__gh__get_issue","mcp__gh__create_issue"]}`
	ap := &approvals{answer: allowAs("UAPP")}
	w, _, stdin, _ := fakeStream(t, mcpTools(ModeFix), "in", init,
		toolUse("t1", "mcp__gh__create_issue"), request("r1", "t1", "mcp__gh__create_issue", `{"title":"x"}`), "in", toolResult("t1", false, "created #7"), resultLine)
	res, err := w.Work(context.Background(), Task{ID: testTask, Transcript: "x", Approve: ap.approve})
	if err != nil || len(ap.asked) != 1 || ap.asked[0].Display != `{"title":"x"}` {
		t.Fatalf("err %v, asked %+v", err, ap.asked)
	}
	if _, b, _ := response(stdin()[1]); b != "allow" || len(res.Actions) != 1 || res.Actions[0] != (Action{Tool: "mcp__gh__create_issue", Input: `{"title":"x"}`, By: "UAPP", Outcome: "ran"}) {
		t.Fatalf("answer %q, actions %+v", b, res.Actions)
	}
	w, _, _, _ = fakeStream(t, mcpTools(ModeFix), "in", init, toolUse("t1", "mcp__gh__create_issue"), toolResult("t1", true, "failed"), resultLine)
	if _, err := w.Work(context.Background(), Task{ID: testTask, Transcript: "x"}); err == nil || !strings.Contains(err.Error(), "without a decision") {
		t.Fatalf("an undecided MCP call passed: %v", err)
	}
}
