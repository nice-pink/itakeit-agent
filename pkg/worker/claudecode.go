package worker

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
)

// callTimeout bounds one CLI run, so a hung process cannot hold a max_parallel
// slot forever.
const callTimeout = 15 * time.Minute

// NewClaudeCode runs every request through the Claude Code CLI in print mode, so
// it uses Claude Code's login: `claude auth login` on this machine, or
// CLAUDE_CODE_OAUTH_TOKEN from `claude setup-token` in a container.
//
// Without tools, each run gets no tools, no MCP servers, no saved session, and
// no user or project settings or CLAUDE.md (--setting-sources ""). Managed
// settings still apply. The CLI still adds an environment block to the prompt
// (login email, working directory, OS), so secret, typically that email, is
// removed from every reply and triage reason. With tools (t non-nil), work
// rounds run as stream sessions in which this process decides every tool call
// (stream.go); triage and the probe stay without tools.
//
// The CLI gets only the environment CLIEnv lets through, so the agent's Slack
// tokens and any other variable not allowed there never reach it. env names
// extra variables to pass (agent.env).
//
// dir holds control/, the system prompt files, and with tools tasks/, one
// working directory per task. No-tools runs use dir itself as their cwd.
func NewClaudeCode(bin, model, skills, dir string, env []string, t *Tools, secret ...string) (*Claude, error) {
	control := filepath.Join(dir, "control")
	if err := os.MkdirAll(control, 0o700); err != nil {
		return nil, err
	}
	c := &cli{bin: bin, model: model, dir: dir, env: env, prompts: &promptFiles{dir: control, paths: map[[32]byte]string{}}, tools: t}
	w := &Claude{ask: c.ask, skills: skills, secret: append(secret, envSecrets(env)...)}
	if t != nil && len(t.MCP) > 0 {
		raw, values, err := mcpConfig(t.MCP)
		if err != nil {
			return nil, err
		}
		c.mcp = &mcpFile{path: filepath.Join(control, "mcp.json"), raw: raw}
		if err := c.mcp.ensure(); err != nil {
			return nil, err
		}
		w.secret = append(w.secret, values...)
	}
	if t != nil {
		w.tools, w.tasks, w.act, w.probeTools = t, filepath.Join(dir, "tasks"), c.act, c.probeTools
		w.lastInit = func() []string { c.mu.Lock(); defer c.mu.Unlock(); return slices.Clone(c.lastInit) }
		for _, d := range []string{w.tasks, filepath.Join(dir, "home")} {
			if err := os.MkdirAll(d, 0o700); err != nil {
				return nil, err
			}
		}
	}
	return w, nil
}

// envSecrets are the values the CLI gets that a tool could print: every
// CLAUDE_CODE_* variable (the OAuth token, a parent Claude Code session's
// tokens) and the agent.env ones. Short values (flags like 1) are left out, so
// scrubbing does not mangle replies.
func envSecrets(extra []string) []string {
	var out []string
	for _, kv := range CLIEnv(extra) {
		name, value, _ := strings.Cut(kv, "=")
		if len(value) >= 8 && (strings.HasPrefix(name, "CLAUDE_CODE_") || slices.Contains(extra, name)) {
			out = append(out, value)
		}
	}
	return out
}

// cli runs the claude binary for one Claude.
type cli struct {
	bin, model, dir string
	env             []string
	prompts         *promptFiles
	tools           *Tools
	mcp             *mcpFile // nil without MCP servers

	mu       sync.Mutex
	unlisted []string // MCP tools no entry names: from the probe, and any a round saw since
	lastInit []string // the last round's tools, as its init line listed them
}

// noteUnlisted adds the MCP tools a session had that no entry names, so the
// next rounds disallow them. The probe finds a server's tools at startup; a
// tool a server adds later is caught here, after one round in which the model
// could see it (not run it: decide denies what no entry names).
func (c *cli) noteUnlisted(initTools []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastInit = slices.Clone(initTools)
	for _, name := range c.tools.unlistedMCP(initTools) {
		if !slices.Contains(c.unlisted, name) {
			slog.Warn("MCP tool on no allow-list: disallowed from the next round", "tool", name)
			c.unlisted = append(c.unlisted, name)
		}
	}
}

func (c *cli) unlistedNow() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.unlisted)
}

// mcpFile is dir/control/mcp.json: the MCP servers with their secrets, which
// is why it is 0600 and outside every task directory. It is checked against
// its content before every session and written again when changed: a Bash
// write could otherwise edit it and make the next session start any command
// as an "MCP server".
type mcpFile struct {
	path string
	raw  []byte
	mu   sync.Mutex
}

// ensure writes through a new file and a rename, so a symlink planted at the
// path is replaced rather than followed, and the mode is always 0600.
func (m *mcpFile) ensure() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if st, err := os.Lstat(m.path); err == nil && st.Mode().IsRegular() && st.Mode().Perm() == 0o600 {
		if got, err := os.ReadFile(m.path); err == nil && bytes.Equal(got, m.raw) {
			return nil
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(m.path), "mcp-*.json") // 0600
	if err != nil {
		return fmt.Errorf("write mcp.json: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(m.raw); err != nil {
		tmp.Close()
		return fmt.Errorf("write mcp.json: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write mcp.json: %w", err)
	}
	if err := os.Rename(tmp.Name(), m.path); err != nil {
		return fmt.Errorf("write mcp.json: %w", err)
	}
	return nil
}

// mcpConfig renders the servers as the CLI's --mcp-config, with the values of
// the variables they name, which it also returns for scrubbing. An unset
// variable fails startup rather than a tool call later.
func mcpConfig(servers map[string]MCPServer) ([]byte, []string, error) {
	var values []string
	get := func(server, name string) (string, error) {
		v, ok := os.LookupEnv(name)
		if !ok || v == "" {
			return "", fmt.Errorf("agent.mcp_servers.%s: %s is not set in the agent's environment", server, name)
		}
		if len(v) >= 8 {
			values = append(values, v)
		}
		return v, nil
	}
	out := map[string]any{}
	for name, s := range servers {
		switch s.Type {
		case "stdio":
			env := map[string]string{}
			for _, k := range s.Env {
				v, err := get(name, k)
				if err != nil {
					return nil, nil, err
				}
				env[k] = v
			}
			out[name] = map[string]any{"type": "stdio", "command": s.Command, "args": s.Args, "env": env}
		case "http":
			headers := map[string]string{}
			for h, k := range s.Headers {
				v, err := get(name, k)
				if err != nil {
					return nil, nil, err
				}
				headers[h] = v
			}
			out[name] = map[string]any{"type": "http", "url": s.URL, "headers": headers}
		default:
			return nil, nil, fmt.Errorf("agent.mcp_servers.%s: type must be stdio or http", name)
		}
	}
	raw, err := json.Marshal(map[string]any{"mcpServers": out})
	return raw, values, err
}

// cliEnvNames are the variables every claude run gets when set: what the CLI
// needs to find its binaries and config, and to reach the API through a
// proxy or a private CA. Bedrock, Vertex and gateway setups need more (AWS_*,
// GOOGLE_*, ANTHROPIC_VERTEX_*, ANTHROPIC_CUSTOM_HEADERS, ...), which the
// operator lists in agent.env. Everything else in this process's environment, such as
// AGENT_SLACK_BOT_TOKEN, is left out. ANTHROPIC_API_KEY and ANTHROPIC_AUTH_TOKEN
// stay out on purpose: the CLI would prefer them over its login and bill the API.
var cliEnvNames = []string{
	"PATH", "HOME", "USER", "LOGNAME", "LANG", "TMPDIR",
	"CLAUDE_CONFIG_DIR", "ANTHROPIC_BASE_URL",
	"HTTPS_PROXY", "HTTP_PROXY", "NO_PROXY", "ALL_PROXY", "https_proxy", "http_proxy", "no_proxy", "all_proxy",
	"NODE_EXTRA_CA_CERTS", "SSL_CERT_FILE", "SSL_CERT_DIR",
}

// CLIPasses reports whether every claude run gets the variable name, so a
// secret must not be kept in it.
func CLIPasses(name string) bool {
	return slices.Contains(cliEnvNames, name) || strings.HasPrefix(name, "CLAUDE_CODE_") || strings.HasPrefix(name, "DISABLE_")
}

// CLIEnv is the environment for every claude run: the variables in cliEnvNames,
// every CLAUDE_CODE_* variable (the CLI's own settings and the
// CLAUDE_CODE_OAUTH_TOKEN login), every DISABLE_* variable (the CLI's opt-outs
// such as DISABLE_TELEMETRY, which --setting-sources "" leaves no other way to
// set), and extra, each only when set in this process.
func CLIEnv(extra []string) []string {
	return slices.DeleteFunc(os.Environ(), func(kv string) bool {
		name, _, _ := strings.Cut(kv, "=")
		return !CLIPasses(name) && !slices.Contains(extra, name)
	})
}

// cliResult is the part of `claude -p --output-format json` the agent reads,
// and of the result line of a stream session.
type cliResult struct {
	IsError          bool            `json:"is_error"`
	Subtype          string          `json:"subtype"`
	StopReason       string          `json:"stop_reason"`
	Result           string          `json:"result"`
	StructuredOutput json.RawMessage `json:"structured_output"`
}

// answer parses the structured output into dest, or says why there is none.
func (res cliResult) answer(dest any) error {
	switch {
	case res.StopReason == "refusal":
		return errRefused
	case res.IsError || res.Subtype != "success":
		return fmt.Errorf("claude: %s", cmp.Or(firstLine(res.Result), res.Subtype))
	case len(res.StructuredOutput) == 0 || string(res.StructuredOutput) == "null":
		return fmt.Errorf("claude: no structured output: %s", firstLine(res.Result))
	}
	if err := json.Unmarshal(res.StructuredOutput, dest); err != nil {
		return fmt.Errorf("parse answer: %w", err)
	}
	return nil
}

// promptFiles writes each distinct system prompt to a file in dir, since the
// prompt carries the knowledge files and one argument is limited to 128 KB on
// Linux. The file is checked against its hash on every use and written again
// when it is missing (a tmp cleaner on an idle host) or changed, so an edit by
// anything else never reaches a later run. Calls run concurrently, hence the
// lock.
type promptFiles struct {
	dir   string
	mu    sync.Mutex
	paths map[[32]byte]string
}

func (p *promptFiles) file(text string) (string, error) {
	key := sha256.Sum256([]byte(text))
	p.mu.Lock()
	defer p.mu.Unlock()
	if path, ok := p.paths[key]; ok {
		if raw, err := os.ReadFile(path); err == nil && sha256.Sum256(raw) == key {
			return path, nil
		}
	}
	path := filepath.Join(p.dir, fmt.Sprintf("system-%x.md", key[:8]))
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		return "", fmt.Errorf("write system prompt: %w", err)
	}
	p.paths[key] = path
	return path, nil
}

func (c *cli) ask(ctx context.Context, system, user, effort string, _ int64, dest any) error {
	schema, err := schemaOf(dest)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	sysFile, err := c.prompts.file(system)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, c.bin, "-p", "--output-format", "json", "--no-session-persistence",
		"--tools", "", "--strict-mcp-config", "--setting-sources", "",
		"--model", c.model, "--effort", effort, "--system-prompt-file", sysFile, "--json-schema", schema)
	cmd.Dir = c.dir
	cmd.Env = CLIEnv(c.env)
	cmd.WaitDelay = 10 * time.Second    // do not wait on a child that holds stdout open
	cmd.Stdin = strings.NewReader(user) // stdin, not an argument: threads can exceed ARG_MAX
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := startTracked(cmd)
	if runErr == nil {
		runErr = cmd.Wait()
		untrack(cmd)
	}

	if ctx.Err() != nil {
		return fmt.Errorf("claude: %w", ctx.Err()) // timed out or shutting down
	}
	var res cliResult
	if err := json.Unmarshal(stdout.Bytes(), &res); err != nil {
		if runErr != nil {
			return fmt.Errorf("claude: %w: %s", runErr, firstLine(stderr.String()+stdout.String()))
		}
		return fmt.Errorf("claude: unreadable output: %w", err)
	}
	return res.answer(dest)
}

// act runs one work round with tools in cwd, the task's directory. In fix mode
// the session gets a fresh, empty HOME: the CLI sources a snapshot of the
// user's shell rc file before every Bash command, so one allowed write to
// ~/.bashrc would otherwise turn every later read entry into arbitrary code.
// A HOME per session lets nothing a session writes outside its cwd reach the
// next one. It needs a login that does not live in HOME (the startup check in
// cmd/itakeit-agent). Propose mode cannot write, so it keeps the real HOME.
func (c *cli) act(ctx context.Context, system, user, effort, cwd string, approve Approve, dest any) (streamResult, error) {
	s, err := c.stream(system, user, effort, cwd, c.tools.exposed(), c.tools.argv(c.mcpPath(), c.unlistedNow()), c.tools.WorkTimeout, dest)
	if err != nil {
		return streamResult{}, err
	}
	if c.tools.Mode == ModeFix {
		home, err := os.MkdirTemp(filepath.Join(c.dir, "home"), filepath.Base(cwd)+"-")
		if err != nil {
			return streamResult{}, err
		}
		defer os.RemoveAll(home)
		s.env = freshHome(s.env, home)
	}
	s.decide = func(tool string, input json.RawMessage) (Verdict, string) { return c.tools.decide(tool, input, cwd) }
	s.approve = approve
	sr, err := s.run(ctx, dest)
	c.noteUnlisted(sr.initTools)
	return sr, err
}

// freshHome points HOME at home and drops CLAUDE_CONFIG_DIR, which would
// move the CLI's config home (shell snapshots, .claude.json) back out of it.
func freshHome(env []string, home string) []string {
	out := slices.DeleteFunc(slices.Clone(env), func(kv string) bool {
		return strings.HasPrefix(kv, "HOME=") || strings.HasPrefix(kv, "CLAUDE_CONFIG_DIR=")
	})
	return append(out, "HOME="+home)
}

func (c *cli) stream(system, user, effort, cwd string, tools, extra []string, timeout time.Duration, dest any) (*stream, error) {
	schema, err := schemaOf(dest)
	if err != nil {
		return nil, err
	}
	sysFile, err := c.prompts.file(system)
	if err != nil {
		return nil, err
	}
	if c.mcp != nil {
		if err := c.mcp.ensure(); err != nil {
			return nil, err
		}
	}
	// CLAUDE_CODE_SUBPROCESS_ENV_SCRUB makes the CLI strip credential variables
	// (the OAuth token, *_TOKEN and cloud keys; verified live for a *_TOKEN) from
	// the Bash tool's shell. Defence in depth only: the CLI's own
	// /proc/<pid>/environ stays readable to the same user, so scrub (worker.go)
	// is the control that holds.
	env := append(CLIEnv(c.env), "CLAUDE_CODE_SUBPROCESS_ENV_SCRUB=1")
	return &stream{bin: c.bin, model: c.model, cwd: cwd, sysFile: sysFile, schema: schema, effort: effort, user: user,
		env: env, tools: tools, extra: extra, timeout: timeout, mcpServers: c.tools.servers()}, nil
}

// notConnected are the configured servers whose status is not connected.
func notConnected(servers []string, status map[string]string) []string {
	var out []string
	for _, s := range servers {
		if status[s] != "connected" {
			out = append(out, s+" ("+cmpOr(status[s], "missing")+")")
		}
	}
	return out
}

func (c *cli) mcpPath() string {
	if c.mcp == nil {
		return ""
	}
	return c.mcp.path
}

const probeToolsPrompt = `You are being checked at startup by the program that runs you. Your answer is never shown to anyone. Do what the user message asks with the Bash tool, then say in one sentence what happened.`

// probeTools checks that permission requests reach this process. Without that
// (a CLI that dropped the hidden --permission-prompt-tool flag, say) every
// tool call would be denied silently, with nothing in the logs saying why. The
// model may answer without calling the tool, so one retry is allowed.
//
// In fix mode it runs with a fresh HOME, as every fix-mode session does, so a
// login that lives in HOME fails here rather than on the first task.
func (c *cli) probeTools(ctx context.Context) error {
	cwd := filepath.Join(c.dir, "probe")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		return err
	}
	defer os.RemoveAll(cwd)
	var home string
	if c.tools.Mode == ModeFix {
		var err error
		if home, err = os.MkdirTemp(filepath.Join(c.dir, "home"), "probe-"); err != nil {
			return err
		}
		defer os.RemoveAll(home)
	}
	// With MCP servers the probe also loads them, with an ask rule and no
	// disallow list, so the init line names all their tools: the unlisted ones
	// are disallowed in every later session.
	rules := []string{"Bash"}
	for _, s := range c.tools.servers() {
		rules = append(rules, "mcp__"+s)
	}
	ask, _ := json.Marshal(map[string]any{"permissions": map[string]any{"ask": rules}})
	extra := []string{"--tools", "Bash", "--settings", string(ask)}
	if c.mcp != nil {
		extra = append(extra, "--mcp-config", c.mcp.path)
	}
	var lastErr, mcpErr error
	for range 2 {
		var out struct {
			Summary string `json:"summary"`
		}
		s, err := c.stream(probeToolsPrompt, "Run the Bash command `echo probe`.", "low", cwd, []string{"Bash"}, extra, 5*time.Minute, &out)
		if err != nil {
			return err
		}
		if home != "" {
			s.env = freshHome(s.env, home)
		}
		s.decide = func(string, json.RawMessage) (Verdict, string) { return Deny, "startup check: not run" }
		sr, err := s.run(ctx, &out)
		// Every server must be connected here: a tool list learned from a server
		// that was still starting would leave its other tools exposed.
		if missing := notConnected(c.tools.servers(), sr.mcpStatus); len(missing) > 0 && sr.requests > 0 {
			mcpErr = fmt.Errorf("MCP servers not connected at startup: %s", strings.Join(missing, ", "))
			continue
		}
		if sr.requests > 0 {
			c.noteUnlisted(sr.initTools)
			return nil
		}
		lastErr = err
	}
	if mcpErr != nil { // kept over a later attempt's error, which would hide it
		return fmt.Errorf("tools probe: %w: the agent needs each server's tool list to disallow the unlisted ones", mcpErr)
	}
	if home != "" {
		return fmt.Errorf("tools probe with an empty HOME failed (%v): fix mode runs every tool session with a fresh HOME, so the login cannot live there; set CLAUDE_CODE_OAUTH_TOKEN from `claude setup-token`", lastErr)
	}
	if lastErr != nil {
		return fmt.Errorf("tools probe: no permission request reached the agent (%w): check the claude CLI version", lastErr)
	}
	return errors.New("tools probe: no permission request reached the agent: check the claude CLI version")
}

// schemaOf renders dest's type as the JSON schema the API backend sends, so both
// backends get the same schema from the same struct.
func schemaOf(dest any) (string, error) {
	raw, err := json.Marshal(anthropic.BetaJSONOutputFormatParam{Schema: dest})
	if err != nil {
		return "", err
	}
	var f struct {
		Schema json.RawMessage `json:"schema"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return "", err
	}
	return string(f.Schema), nil
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return s
}
