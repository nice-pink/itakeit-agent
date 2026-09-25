package agent

import (
	"cmp"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

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
	ItakeitUser       string `yaml:"itakeit_user"`
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
	if s.ItakeitUser == "" {
		return nil, errors.New("config: agent.itakeit_user is required (the itakeit bot's user ID, so its board and cards are not taken for tasks)")
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
	if s.MaxParallel == 0 {
		s.MaxParallel = 2
	}
	if s.RecoverMessages == 0 {
		s.RecoverMessages = 200
	}
	return &Config{Config: base, Agent: s}, nil
}

func (c *Config) ClaimDelay() time.Duration {
	return time.Duration(c.Agent.ClaimDelaySeconds) * time.Second
}
