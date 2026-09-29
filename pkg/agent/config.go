package agent

import (
	"cmp"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/nice-pink/itakeit-agent/pkg/worker"
	"github.com/nice-pink/itakeit/pkg/config"
	"github.com/nice-pink/itakeit/pkg/task"
	"gopkg.in/yaml.v3"
)

// Config is itakeit's own config file plus an agent block, so both apps read the
// channel, the emoji and status_claims from one file and cannot disagree on
// which reaction means what. itakeit ignores the agent block.
type Config struct {
	*config.Config
	Agent Settings `yaml:"agent"`
}

type Settings struct {
	// ItakeitUser (the itakeit bot's member ID) or ItakeitApp (the itakeit
	// app's App ID) identifies itakeit's own messages, so its board and cards
	// are not taken for tasks. Slack shows no member ID for an app, so the App
	// ID is the one people can find; one of the two is required.
	ItakeitUser       string `yaml:"itakeit_user"`
	ItakeitApp        string `yaml:"itakeit_app"`
	Backend           string `yaml:"backend"`
	ClaudeBin         string `yaml:"claude_bin"`
	Model             string `yaml:"model"`
	Skills            string `yaml:"skills"`
	ClaimDelaySeconds int    `yaml:"claim_delay_seconds"`
	ClaimOwned        bool   `yaml:"claim_owned"`
	MaxParallel       int    `yaml:"max_parallel"`
	RecoverMessages   int    `yaml:"recover_messages"`
	// Knowledge lists Markdown or text files, relative to the directory of the
	// -config path, whose contents go into every triage and work request. Load
	// fills KnowledgeText.
	Knowledge     []string `yaml:"knowledge"`
	KnowledgeText string   `yaml:"-"`
	// Env names extra variables passed to the claude CLI, which otherwise gets
	// only the few it needs (worker.CLIEnv).
	Env []string `yaml:"env"`
	// Mode, Tools and the rest give work rounds tools (claude-code only). Parse
	// fills ToolEntries and WriteEntries from Tools.
	Mode  string `yaml:"mode"`
	Tools struct {
		Read  []string `yaml:"read"`
		Write []string `yaml:"write"`
	} `yaml:"tools"`
	// Approver is the Slack user whose reaction every write needs in fix mode.
	Approver              string `yaml:"approver"`
	AllowUnapprovedWrites bool   `yaml:"allow_unapproved_writes"`
	ApprovalEmoji         struct {
		Approve string `yaml:"approve"`
		Deny    string `yaml:"deny"`
	} `yaml:"approval_emoji"`
	ApprovalTimeoutMinutes int `yaml:"approval_timeout_minutes"`
	WorkTimeoutMinutes     int `yaml:"work_timeout_minutes"`
	// MCPServers are started or connected to for tool rounds; entries name
	// their tools as mcp__<server>__<tool>.
	MCPServers map[string]MCPServer `yaml:"mcp_servers"`
	// MaxSessions bounds tool rounds, running or waiting for approval.
	MaxSessions  int            `yaml:"max_sessions"`
	ToolEntries  []worker.Entry `yaml:"-"`
	WriteEntries []worker.Entry `yaml:"-"`
	// Memory is the searchable memory of past tasks (poma-memory).
	Memory Memory `yaml:"memory"`
}

// Memory is the agent.memory block. Dir is relative to the directory of the
// -config path, and Load makes it absolute.
type Memory struct {
	Enabled bool   `yaml:"enabled"`
	Dir     string `yaml:"dir"`
	Bin     string `yaml:"bin"`
	Results int    `yaml:"results"`
	// Approval makes every learning wait for agent.approver's reaction before
	// it is saved. Default true: learnings come from Slack threads, and a saved
	// one reaches every later task it matches.
	Approval *bool `yaml:"approval"`
}

// NeedsApproval reports whether learnings wait for the approver.
func (m Memory) NeedsApproval() bool { return m.Enabled && (m.Approval == nil || *m.Approval) }

// MCPServer is one agent.mcp_servers entry. Env and Headers name variables of
// the agent's environment, whose values reach the server through mcp.json.
type MCPServer struct {
	Type    string            `yaml:"type"`
	Command string            `yaml:"command"`
	Args    []string          `yaml:"args"`
	Env     []string          `yaml:"env"`
	URL     string            `yaml:"url"`
	Headers map[string]string `yaml:"headers"` // header name: variable holding its value
}

// WorkerTools is the tool setup for the worker, nil when no tools are listed.
func (s Settings) WorkerTools() *worker.Tools {
	if len(s.ToolEntries)+len(s.WriteEntries) == 0 {
		return nil
	}
	t := &worker.Tools{Mode: worker.Mode(s.Mode), Read: s.ToolEntries, Write: s.WriteEntries, Approval: s.Approver != "",
		WorkTimeout: time.Duration(s.WorkTimeoutMinutes) * time.Minute}
	if len(s.MCPServers) > 0 {
		t.MCP = map[string]worker.MCPServer{}
		for name, m := range s.MCPServers {
			t.MCP[name] = worker.MCPServer{Type: m.Type, Command: m.Command, Args: m.Args, Env: m.Env, URL: m.URL, Headers: m.Headers}
		}
	}
	return t
}

// maxKnowledge caps the knowledge files, since they are sent with every call.
// About 64K tokens.
const maxKnowledge = 256 << 10

// Backends for the model calls.
const (
	BackendClaudeCode = "claude-code" // the Claude Code CLI and its login
	BackendAPI        = "api"         // the Messages API with API credentials
)

func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c, err := Parse(raw)
	if err != nil {
		return nil, err
	}
	if m := &c.Agent.Memory; m.Enabled {
		if !filepath.IsAbs(m.Dir) {
			m.Dir = filepath.Join(filepath.Dir(path), m.Dir)
		}
		if m.Dir, err = filepath.Abs(m.Dir); err != nil {
			return nil, err
		}
	}
	c.Agent.KnowledgeText, err = readKnowledge(filepath.Dir(path), c.Agent.Knowledge)
	return c, err
}

// readKnowledge joins the files, each under a line with its path as listed, so
// the model can tell them apart. Sizes are checked before reading, so a file
// listed by mistake (a large log) is refused without loading it.
func readKnowledge(dir string, files []string) (string, error) {
	var b strings.Builder
	for _, name := range files {
		path := name
		if !filepath.IsAbs(path) {
			path = filepath.Join(dir, path)
		}
		st, err := os.Stat(path)
		if err != nil {
			return "", fmt.Errorf("config: agent.knowledge: %w", err)
		}
		if total := int64(b.Len()+len(name)+16) + st.Size(); total > maxKnowledge { // 16: header and separator
			return "", fmt.Errorf("config: agent.knowledge reaches %d KB with %s, over the %d KB limit. It is sent with every triage and work request: trim it", total>>10, name, maxKnowledge>>10)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("config: agent.knowledge: %w", err)
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "===== %s =====\n%s", name, strings.TrimSpace(string(raw)))
	}
	return b.String(), nil
}

func Parse(raw []byte) (*Config, error) {
	base, err := config.Parse(raw)
	if err != nil {
		return nil, err
	}
	var file struct {
		Agent Settings `yaml:"agent"`
	}
	if err := yaml.Unmarshal(raw, &file); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	for _, act := range []task.Action{task.InProgress, task.NeedsInfo, task.Blocked, task.Done} {
		if len(base.Emoji[act]) == 0 {
			return nil, fmt.Errorf("config: the agent needs an emoji for %s: it reports every round with in_progress, needs_info, blocked and done", act)
		}
	}
	s := file.Agent
	switch {
	case s.ItakeitUser == "" && s.ItakeitApp == "":
		return nil, errors.New("config: agent.itakeit_app is required (the itakeit app's App ID, A..., from api.slack.com/apps -> itakeit -> Basic Information), so its board and cards are not taken for tasks. agent.itakeit_user, the itakeit bot's member ID, works instead")
	case s.ItakeitApp != "" && !slackApp.MatchString(s.ItakeitApp):
		return nil, fmt.Errorf("config: agent.itakeit_app must be a Slack App ID (A...), got %q", s.ItakeitApp)
	case s.ItakeitUser != "" && !slackUser.MatchString(s.ItakeitUser):
		return nil, fmt.Errorf("config: agent.itakeit_user must be a Slack member ID (U... or W...), got %q: for the App ID (A...) use agent.itakeit_app", s.ItakeitUser)
	}
	if s.Skills == "" {
		return nil, errors.New("config: agent.skills is required (what the agent can do, used to decide which tasks to take)")
	}
	switch s.Backend {
	case "", BackendClaudeCode:
		s.Backend = BackendClaudeCode
		s.Model = cmp.Or(s.Model, "opus")
		s.ClaudeBin = cmp.Or(s.ClaudeBin, "claude")
	case BackendAPI:
		s.Model = cmp.Or(s.Model, "claude-opus-5")
	default:
		return nil, fmt.Errorf("config: agent.backend must be %q or %q, got %q", BackendClaudeCode, BackendAPI, s.Backend)
	}
	if s.ClaimDelaySeconds < 0 || s.MaxParallel < 0 || s.RecoverMessages < 0 {
		return nil, errors.New("config: agent.claim_delay_seconds, max_parallel and recover_messages must be positive")
	}
	for _, name := range s.Env {
		if !envName.MatchString(name) {
			return nil, fmt.Errorf("config: agent.env: %q is not a variable name", name)
		}
		// Compared in upper case: names are case-sensitive on Linux and macOS, but
		// Windows folds them.
		if up := strings.ToUpper(name); strings.HasPrefix(up, "AGENT_SLACK_") || up == "ANTHROPIC_API_KEY" || up == "ANTHROPIC_AUTH_TOKEN" {
			return nil, fmt.Errorf("config: agent.env must not pass %s to the claude CLI", name)
		}
	}
	if s.MaxParallel == 0 {
		s.MaxParallel = 2
	}
	if err := parseTools(&s, base.Emoji); err != nil {
		return nil, err
	}
	if s.RecoverMessages == 0 {
		s.RecoverMessages = 200
	}
	if err := parseMemory(&s); err != nil {
		return nil, err
	}
	return &Config{Config: base, Agent: s}, nil
}

func parseMemory(s *Settings) error {
	m := &s.Memory
	if !m.Enabled {
		return nil
	}
	m.Dir = cmp.Or(m.Dir, "memory")
	m.Bin = cmp.Or(m.Bin, "poma-memory")
	if strings.Contains(m.Bin, "/") && !filepath.IsAbs(m.Bin) {
		return errors.New("config: agent.memory.bin must be absolute or a name on PATH")
	}
	switch {
	case m.Results == 0:
		m.Results = 3
	case m.Results < 1 || m.Results > 10:
		return errors.New("config: agent.memory.results must be between 1 and 10")
	}
	if m.NeedsApproval() && s.Approver == "" {
		return errors.New("config: agent.memory.approval needs agent.approver, whose reaction saves each learning: set it, or agent.memory.approval: false to save learnings unreviewed")
	}
	return nil
}

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

var slackUser = regexp.MustCompile(`^[UW][A-Z0-9]{2,}$`)

var slackApp = regexp.MustCompile(`^A[A-Z0-9]{2,}$`)

// headerName is an HTTP header field name (RFC 9110 token).
var headerName = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")

// mcpServerName has no underscores, so mcp__<server>__<tool> splits cleanly.
var mcpServerName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// checkMCP validates the MCP servers and the entries that name their tools.
func checkMCP(s *Settings) error {
	for name, m := range s.MCPServers {
		if !mcpServerName.MatchString(name) {
			return fmt.Errorf("config: agent.mcp_servers: %q must be lowercase letters, digits and dashes (no underscores)", name)
		}
		switch m.Type {
		case "stdio":
			if m.Command == "" || m.URL != "" || len(m.Headers) > 0 {
				return fmt.Errorf("config: agent.mcp_servers.%s: stdio needs command, and no url or headers", name)
			}
			// The CLI starts a server in the task's directory, which rounds can
			// write to: a relative command would run a file a round wrote there.
			if strings.Contains(m.Command, "/") && !filepath.IsAbs(m.Command) {
				return fmt.Errorf("config: agent.mcp_servers.%s: command must be absolute or a name on PATH", name)
			}
		case "http":
			u, err := url.Parse(m.URL)
			if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || m.Command != "" || len(m.Env) > 0 {
				return fmt.Errorf("config: agent.mcp_servers.%s: http needs an https:// url with a host and no credentials in it (use headers), and no command or env", name)
			}
			for h := range m.Headers {
				if !headerName.MatchString(h) {
					return fmt.Errorf("config: agent.mcp_servers.%s: %q is not a header name", name, h)
				}
			}
		default:
			return fmt.Errorf("config: agent.mcp_servers.%s: type must be stdio or http", name)
		}
		vars := slices.Clone(m.Env)
		for _, v := range m.Headers {
			vars = append(vars, v)
		}
		for _, v := range vars {
			if !envName.MatchString(v) {
				return fmt.Errorf("config: agent.mcp_servers.%s: %q is not a variable name", name, v)
			}
			if up := strings.ToUpper(v); strings.HasPrefix(up, "AGENT_SLACK_") || up == "ANTHROPIC_API_KEY" || up == "ANTHROPIC_AUTH_TOKEN" {
				return fmt.Errorf("config: agent.mcp_servers.%s must not pass %s", name, v)
			}
			// A variable the CLI gets anyway (agent.env, CLAUDE_CODE_*, proxies ...)
			// would sit in its environment, where a Bash tool can print it.
			if slices.Contains(s.Env, v) || worker.CLIPasses(v) {
				return fmt.Errorf("config: agent.mcp_servers.%s: %s also reaches the CLI's environment (agent.env or always passed), which would expose it to the CLI's tools: use another variable", name, v)
			}
		}
	}
	if len(s.MCPServers) > 0 && len(s.ToolEntries)+len(s.WriteEntries) == 0 {
		return errors.New("config: agent.mcp_servers needs agent.tools entries: without them no round has tools")
	}
	for _, e := range append(slices.Clone(s.ToolEntries), s.WriteEntries...) {
		if srv := e.MCPServer(); srv != "" {
			if _, ok := s.MCPServers[srv]; !ok {
				return fmt.Errorf("config: agent.tools: %s names server %s, which agent.mcp_servers does not configure", e, srv)
			}
		}
	}
	return nil
}

// parseTools checks the tool settings. emoji are itakeit's status emoji, which
// the approval emoji must not collide with.
func parseTools(s *Settings, emoji map[task.Action][]string) error {
	switch s.Mode {
	case "", string(worker.ModePropose):
		s.Mode = string(worker.ModePropose)
	case string(worker.ModeFix):
	default:
		return fmt.Errorf("config: agent.mode must be %q or %q, got %q", worker.ModePropose, worker.ModeFix, s.Mode)
	}
	for _, raw := range s.Tools.Read {
		e, err := worker.ParseEntry(raw, false)
		if err != nil {
			return fmt.Errorf("config: agent.tools.read: %w", err)
		}
		s.ToolEntries = append(s.ToolEntries, e)
	}
	for _, raw := range s.Tools.Write {
		e, err := worker.ParseEntry(raw, true)
		if err != nil {
			return fmt.Errorf("config: agent.tools.write: %w", err)
		}
		for _, r := range s.ToolEntries {
			if worker.Covers(r, e) {
				return fmt.Errorf("config: agent.tools: read entry %s also covers write entry %s, which would then run without approval: narrow the read entry", r, e)
			}
		}
		if e.NeedsApprover() && s.Approver == "" {
			return fmt.Errorf("config: agent.tools.write: %s can run anything, so it needs agent.approver", e)
		}
		s.WriteEntries = append(s.WriteEntries, e)
	}
	// An approver on the API backend only approves learnings.
	if s.Backend != BackendClaudeCode && (len(s.ToolEntries)+len(s.WriteEntries) > 0 || (s.Approver != "" && !s.Memory.NeedsApproval()) || s.Mode == string(worker.ModeFix) || len(s.MCPServers) > 0) {
		return fmt.Errorf("config: agent.tools, agent.mcp_servers, agent.approver (except for agent.memory.approval) and mode fix need backend %s: the API backend has no tools", BackendClaudeCode)
	}
	if err := checkMCP(s); err != nil {
		return err
	}
	if s.Mode == string(worker.ModeFix) {
		switch {
		case len(s.WriteEntries) == 0:
			return errors.New("config: agent.mode fix needs agent.tools.write: the changes the agent may make")
		case s.Approver == "" && !s.AllowUnapprovedWrites:
			return errors.New("config: fix mode without approver lets anyone in the channel trigger write entries: set agent.approver, or agent.allow_unapproved_writes: true")
		}
	}
	if s.Approver != "" {
		if !slackUser.MatchString(s.Approver) {
			return fmt.Errorf("config: agent.approver must be a Slack member ID (U... or W...), got %q", s.Approver)
		}
		if s.Approver == s.ItakeitUser {
			return errors.New("config: agent.approver cannot be the itakeit bot")
		}
	}
	s.ApprovalEmoji.Approve = cmp.Or(s.ApprovalEmoji.Approve, "heavy_check_mark")
	s.ApprovalEmoji.Deny = cmp.Or(s.ApprovalEmoji.Deny, "x")
	if s.ApprovalEmoji.Approve == s.ApprovalEmoji.Deny {
		return errors.New("config: agent.approval_emoji.approve and .deny must differ")
	}
	for act, names := range emoji {
		for _, e := range []string{s.ApprovalEmoji.Approve, s.ApprovalEmoji.Deny} {
			if slices.Contains(names, e) {
				return fmt.Errorf("config: agent.approval_emoji %s is itakeit's %s emoji: approving would change the task's status", e, act)
			}
		}
	}
	switch {
	case s.ApprovalTimeoutMinutes == 0:
		s.ApprovalTimeoutMinutes = 30
	case s.ApprovalTimeoutMinutes < 1 || s.ApprovalTimeoutMinutes > 40:
		// 40: the CLI was verified to keep a permission request open that long.
		return errors.New("config: agent.approval_timeout_minutes must be between 1 and 40")
	}
	switch {
	case s.WorkTimeoutMinutes == 0:
		s.WorkTimeoutMinutes = 15
	case s.WorkTimeoutMinutes < 1 || s.WorkTimeoutMinutes > 120:
		return errors.New("config: agent.work_timeout_minutes must be between 1 and 120")
	}
	switch {
	case s.MaxSessions == 0:
		s.MaxSessions = s.MaxParallel
	case s.MaxSessions < 0:
		return errors.New("config: agent.max_sessions must be positive")
	}
	return nil
}

func (c *Config) ClaimDelay() time.Duration {
	return time.Duration(c.Agent.ClaimDelaySeconds) * time.Second
}
