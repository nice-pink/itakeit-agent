package worker

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
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
// Each run gets no tools, no MCP servers, no saved session, and no user or
// project settings or CLAUDE.md (--setting-sources ""). Managed settings still
// apply. The CLI still adds an environment block to the prompt (login email,
// working directory, OS), so secret, typically that email, is removed from
// every reply and triage reason. dir is the working directory for the runs.
//
// ANTHROPIC_API_KEY and ANTHROPIC_AUTH_TOKEN are removed from the CLI's
// environment: the CLI would prefer them over its login and bill the API.
func NewClaudeCode(bin, model, skills, dir string, secret ...string) *Claude {
	return &Claude{ask: cliAsk(bin, model, dir), skills: skills, secret: secret}
}

// CLIEnv is the environment for every claude run: this process's, without API
// credentials.
func CLIEnv() []string {
	return slices.DeleteFunc(os.Environ(), func(kv string) bool {
		return strings.HasPrefix(kv, "ANTHROPIC_API_KEY=") || strings.HasPrefix(kv, "ANTHROPIC_AUTH_TOKEN=")
	})
}

// cliResult is the part of `claude -p --output-format json` the agent reads.
type cliResult struct {
	IsError          bool            `json:"is_error"`
	Subtype          string          `json:"subtype"`
	StopReason       string          `json:"stop_reason"`
	Result           string          `json:"result"`
	StructuredOutput json.RawMessage `json:"structured_output"`
}

// promptFiles writes each distinct system prompt to a file in dir once, since
// the prompt carries the knowledge files and one argument is limited to 128 KB
// on Linux. Calls run concurrently, hence the lock.
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
		if _, err := os.Stat(path); err == nil {
			return path, nil
		} // removed (a tmp cleaner on an idle host): write it again
	}
	path := filepath.Join(p.dir, fmt.Sprintf("system-%x.md", key[:8]))
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		return "", fmt.Errorf("write system prompt: %w", err)
	}
	p.paths[key] = path
	return path, nil
}

func cliAsk(bin, model, dir string) askFunc {
	prompts := &promptFiles{dir: dir, paths: map[[32]byte]string{}}
	return func(ctx context.Context, system, user, effort string, _ int64, dest any) error {
		schema, err := schemaOf(dest)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(ctx, callTimeout)
		defer cancel()
		sysFile, err := prompts.file(system)
		if err != nil {
			return err
		}
		cmd := exec.CommandContext(ctx, bin, "-p", "--output-format", "json", "--no-session-persistence",
			"--tools", "", "--strict-mcp-config", "--setting-sources", "",
			"--model", model, "--effort", effort, "--system-prompt-file", sysFile, "--json-schema", schema)
		cmd.Dir = dir
		cmd.Env = CLIEnv()
		cmd.WaitDelay = 10 * time.Second    // do not wait on a child that holds stdout open
		cmd.Stdin = strings.NewReader(user) // stdin, not an argument: threads can exceed ARG_MAX
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		runErr := cmd.Run()

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
