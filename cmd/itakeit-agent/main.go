package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/nice-pink/itakeit-agent/pkg/agent"
	"github.com/nice-pink/itakeit-agent/pkg/worker"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"
)

func main() {
	cfgPath := flag.String("config", "config.yaml", "path to the config file (itakeit's, with an agent block)")
	debug := flag.Bool("debug", false, "log raw Socket Mode traffic")
	flag.Parse()

	if err := run(*cfgPath, *debug); err != nil {
		slog.Error("itakeit-agent stopped", "err", err)
		os.Exit(1)
	}
}

func run(cfgPath string, debug bool) error {
	if err := hideEnviron(); err != nil {
		slog.Warn("could not hide this process's environment from its children", "err", err)
	}
	if err := subreaper(); err != nil {
		slog.Warn("could not become a subreaper: processes a tool session leaves behind are not reaped", "err", err)
	}
	cfg, err := agent.Load(cfgPath)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Memory is checked first: the image without poma-memory fails here, before
	// anything else, with the name of the image to run instead.
	var mem *worker.Memory
	if m := cfg.Agent.Memory; m.Enabled {
		mem = worker.NewMemory(m.Bin, m.Dir, m.Results)
		if err := mem.Check(ctx); err != nil {
			if ctx.Err() != nil {
				return nil // stopped during startup: not a failure
			}
			return err
		}
		slog.Info("memory ready", "dir", m.Dir, "results", m.Results, "approval", m.NeedsApproval())
	}
	botToken, appToken := os.Getenv("AGENT_SLACK_BOT_TOKEN"), os.Getenv("AGENT_SLACK_APP_TOKEN")
	if !strings.HasPrefix(botToken, "xoxb-") || !strings.HasPrefix(appToken, "xapp-") {
		return errMissingTokens
	}

	api := slack.New(botToken, slack.OptionAppLevelToken(appToken), slack.OptionDebug(debug),
		slack.OptionHTTPClient(&http.Client{Timeout: 15 * time.Second}), slack.OptionRetry(3))
	auth, err := api.AuthTest()
	if err != nil {
		return err
	}
	if cfg.Agent.Approver != "" && cfg.Agent.Approver == auth.UserID {
		return errors.New("agent.approver is the agent's own user: approvals must come from a person")
	}
	if auth.UserID == cfg.Agent.ItakeitUser || (cfg.Agent.ItakeitApp != "" && appTokenApp(appToken) == cfg.Agent.ItakeitApp) {
		return errors.New("the agent's tokens belong to the itakeit app: create a separate Slack app for the agent, itakeit ignores its own reactions")
	}
	slog.Info("authenticated", "team", auth.Team, "agent_user", auth.UserID, "channels", cfg.Channels, "auto_channels", cfg.AutoChannels, "backend", cfg.Agent.Backend, "model", cfg.Agent.Model)
	if err := agent.CheckChannels(api, cfg); err != nil {
		return err
	}

	dir, err := os.MkdirTemp("", "itakeit-agent-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	w, err := newWorker(ctx, cfg.Agent, dir, mem, botToken, appToken)
	if ctx.Err() != nil {
		return nil // stopped during startup: not a failure
	}
	if err != nil {
		return err
	}

	sm := socketmode.New(api, socketmode.OptionDebug(debug))
	a := agent.New(api, w, cfg, auth.UserID, auth.BotID)
	if mem != nil {
		a.WithMemory(mem) // not a nil *Memory in the interface
	}
	return a.Run(ctx, sm)
}

// newWorker builds the worker. secret are the Slack tokens, removed from
// everything posted in case a tool finds them.
func newWorker(ctx context.Context, s agent.Settings, dir string, mem *worker.Memory, secret ...string) (worker.Worker, error) {
	var w *worker.Claude
	switch s.Backend {
	case agent.BackendAPI:
		w = worker.NewAPI(s.Model, s.Skills)
	case agent.BackendLangdock:
		key := os.Getenv("LANGDOCK_API_KEY")
		if key == "" {
			return nil, errors.New("agent.backend langdock needs LANGDOCK_API_KEY")
		}
		w = worker.NewLangdock(s.LangdockRegion, s.Model, key, s.Skills)
	default:
		email, err := claudeLogin(ctx, s.ClaudeBin, s.Env)
		if err != nil {
			return nil, err
		}
		t := s.WorkerTools()
		if t != nil {
			slog.Info("work rounds get tools", "mode", t.Mode, "read", s.Tools.Read, "write", s.Tools.Write, "mcp_servers", slices.Sorted(maps.Keys(s.MCPServers)), "approver", s.Approver, "work_timeout_minutes", s.WorkTimeoutMinutes, "max_sessions", s.MaxSessions)
			switch {
			case t.Mode == worker.ModeFix && s.Approver == "":
				slog.Warn("fix mode without approver: anyone in the channel can trigger write entries")
			case t.Mode == worker.ModePropose && s.Approver != "" && !s.Memory.NeedsApproval():
				slog.Warn("agent.approver has no effect in propose mode, which never writes")
			}
			if s.Approver != "" && s.AllowUnapprovedWrites {
				slog.Warn("agent.allow_unapproved_writes has no effect with an approver: every write needs the approval")
			}
			switch {
			case s.AllowRealHome && t.Mode == worker.ModeFix:
				slog.Warn("agent.allow_real_home: fix-mode sessions share the agent's HOME, so what one session writes there reaches the next")
			case s.AllowRealHome:
				slog.Warn("agent.allow_real_home has no effect in propose mode, which always keeps the real HOME")
			}
			for _, e := range t.Read {
				if e.Broad() {
					slog.Warn("a read entry names a whole program, which reaches all of its subcommands: name the subcommand", "entry", e.String())
				}
			}
		}
		if w, err = worker.NewClaudeCode(s.ClaudeBin, s.Model, s.Skills, dir, s.Env, t, append(secret, email)...); err != nil {
			return nil, err
		}
	}
	w.Knowledge = s.KnowledgeText
	w.Memory = mem
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	refused, err := w.Probe(ctx)
	if err != nil {
		if s.Backend == agent.BackendClaudeCode {
			return nil, fmt.Errorf("agent.backend %s, model %q: %w (the CLI gets only an allow-listed environment: list any variable it needs, such as Bedrock or Vertex credentials, in agent.env)", s.Backend, s.Model, err)
		}
		return nil, fmt.Errorf("agent.backend %s, model %q: %w", s.Backend, s.Model, err)
	}
	slog.Info("model ready", "backend", s.Backend, "model", s.Model, "knowledge_files", len(s.Knowledge), "knowledge_bytes", len(s.KnowledgeText), "scrubbed_strings", w.Secrets(), "probe_refused", refused)
	if s.Backend == agent.BackendClaudeCode && w.Secrets() == 0 {
		slog.Warn("no login email known, so none is removed from replies: the CLI may still show one to the model")
	}
	return w, nil
}

// claudeLogin fails at start rather than on the first task when the CLI is
// missing or has no login. It counts both `claude auth login` and a
// CLAUDE_CODE_OAUTH_TOKEN in the environment. Whether the login still works is
// the startup probe's job.
// It returns the login's email when auth status reports it (a `claude auth
// login`, not a CLAUDE_CODE_OAUTH_TOKEN), which the CLI shows the model and which
// is therefore removed from replies. The startup probe tries to find it for either.
func claudeLogin(ctx context.Context, bin string, env []string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "auth", "status")
	cmd.Env = worker.CLIEnv(env)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("agent.backend claude-code: `%s auth status` failed (%w): install Claude Code and run `claude auth login`, or set CLAUDE_CODE_OAUTH_TOKEN from `claude setup-token`", bin, err)
	}
	var st struct {
		LoggedIn   bool   `json:"loggedIn"`
		AuthMethod string `json:"authMethod"`
		Email      string `json:"email"`
	}
	if json.Unmarshal(out, &st) != nil || !st.LoggedIn {
		return "", errors.New("agent.backend claude-code: Claude Code is not logged in: run `claude auth login`, or set CLAUDE_CODE_OAUTH_TOKEN from `claude setup-token`")
	}
	slog.Info("claude code login", "method", st.AuthMethod)
	return st.Email, nil
}

// appTokenApp is the App ID an app-level token belongs to: they read
// xapp-1-<App ID>-<number>-<secret>. auth.test reports no App ID, and bots.info
// would need users:read.
func appTokenApp(token string) string {
	if parts := strings.Split(token, "-"); len(parts) >= 3 && parts[0] == "xapp" {
		return parts[2]
	}
	return ""
}

var errMissingTokens = errors.New("set AGENT_SLACK_BOT_TOKEN (xoxb-...) and AGENT_SLACK_APP_TOKEN (xapp-...) from the agent's own Slack app")
