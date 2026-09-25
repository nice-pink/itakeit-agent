package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
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
	cfg, err := agent.Load(cfgPath)
	if err != nil {
		return err
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
	if auth.UserID == cfg.Agent.ItakeitUser {
		return errors.New("the agent's tokens belong to the itakeit app: create a separate Slack app for the agent, itakeit ignores its own reactions")
	}
	slog.Info("authenticated", "team", auth.Team, "agent_user", auth.UserID, "channel", cfg.Channel, "backend", cfg.Agent.Backend, "model", cfg.Agent.Model)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	dir, err := os.MkdirTemp("", "itakeit-agent-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	w, err := newWorker(ctx, cfg.Agent, dir)
	if ctx.Err() != nil {
		return nil // stopped during startup: not a failure
	}
	if err != nil {
		return err
	}

	sm := socketmode.New(api, socketmode.OptionDebug(debug))
	return agent.New(api, w, cfg, auth.UserID, auth.BotID).Run(ctx, sm)
}

func newWorker(ctx context.Context, s agent.Settings, dir string) (worker.Worker, error) {
	var w *worker.Claude
	if s.Backend == agent.BackendAPI {
		w = worker.NewAPI(s.Model, s.Skills)
	} else {
		email, err := claudeLogin(ctx, s.ClaudeBin)
		if err != nil {
			return nil, err
		}
		w = worker.NewClaudeCode(s.ClaudeBin, s.Model, s.Skills, dir, email)
	}
	w.Knowledge = s.KnowledgeText
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	refused, err := w.Probe(ctx)
	if err != nil {
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
func claudeLogin(ctx context.Context, bin string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "auth", "status")
	cmd.Env = worker.CLIEnv()
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

var errMissingTokens = errors.New("set AGENT_SLACK_BOT_TOKEN (xoxb-...) and AGENT_SLACK_APP_TOKEN (xapp-...) from the agent's own Slack app")
