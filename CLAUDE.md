# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

Behaviour, setup, config keys and the security caveats are documented in `README.md`. This file covers what you need to change the code.

## Commands

- Build (runs all tests first, then writes `bin/itakeit-agent`): `./build`
- All tests: `go test ./...`
- One test: `go test ./pkg/agent -run TestNeedsInfoResumesOnAnswer -v`
- Run locally: `AGENT_SLACK_BOT_TOKEN=xoxb-... AGENT_SLACK_APP_TOKEN=xapp-... ./bin/itakeit-agent -config config.yaml` (`-debug` logs raw Socket Mode traffic). `config.yaml` is gitignored; start from `config.example.yaml`.
- Image: `docker build -t itakeit-agent .`, with memory `docker build -f Dockerfile.mem -t itakeit-agent-mem .`, for the HTTP backends `Dockerfile.langdock` and `Dockerfile.openai` (keep all four Dockerfiles' runtime stages in step: `Dockerfile.langdock` and `Dockerfile.openai` are identical apart from comments). The runtime stage pins the Claude Code CLI with `CLAUDE_CODE_VERSION`. Bump it only after checking the CLI flags and the `claude -p --output-format json` fields the worker reads still match.

## Architecture

Two packages, wired together in `cmd/itakeit-agent/main.go`.

`pkg/agent` is the Slack side. It talks to itakeit only through reactions and thread replies, never through itakeit's API. The agent is a separate Slack app because itakeit ignores its own bot's reactions.
- One goroutine owns all state (`jobs`, `queued`). Model calls run off the loop through `a.spawn`, which returns a func that is applied back on the loop through `a.do`. Delays go through `a.after`. Never touch agent state from a spawned goroutine. Tests swap `spawn` and `after` for inline calls, so they run synchronously.
- `Config` embeds itakeit's own `config.Config` (`github.com/nice-pink/itakeit`) and adds the `agent:` block, so the channels (`channels`, or `auto_channels`), emoji and `status_claims` come from itakeit's config file. Task statuses are `task.Action` values from that module.
- There is no database. `Recover` rebuilds owned jobs from the agent's own reactions on recent history, per channel.
- Everything is keyed by channel and message ts together (`key`, `split` in `channels.go`), since a ts is unique only within its channel; the key is also the worker's task ID and directory name. With `auto_channels` the served set is `Agent.joined`, kept by join/leave/archive events and a listing every 15 minutes (`relist`).
- Status changes add the new reaction before removing the old one. itakeit takes the latest added status, so reversing the order clears it.
- Replies are escaped (`escape`/`unescape`) so model output cannot ping `@channel` or groups. Transcripts sent to the model are unescaped.
- `API` is the subset of `*slack.Client` in use. `agent_test.go` fakes it with `fakeAPI` and fakes the worker with `fakeWorker`.

`pkg/worker` is the model side. The `Worker` interface (`Triage`, `Work`) is the extension point for an agent that can act on systems.
- `Claude` holds the prompts, the JSON output schemas (derived from struct tags via `jsonschema`), secret scrubbing and knowledge. It calls the model through one `askFunc`, with two backends: `claudecode.go` (`claude -p`, the default) and `api.go` (Messages API, streaming, refusal fallbacks). Backend-specific behaviour belongs in the `askFunc`, not in `Claude`.
- The CLI backend isolates every run: `--tools ""` (tool rounds excepted, below), `--strict-mcp-config`, `--setting-sources ""`, `--no-session-persistence`, and an allow-listed environment (`CLIEnv`, extended by `agent.env`) that never carries the Slack tokens or `ANTHROPIC_API_KEY`/`ANTHROPIC_AUTH_TOKEN`. A test double run as the CLI sees only that environment, so it must name its own variables (see `fakeCLI`; `testdata/fake-env` dumps what the CLI receives). The system prompt goes through `--system-prompt-file` because Linux caps one argument at 128 KB. Each run is killed after 15 minutes, a tool round after `work_timeout_minutes`.
- With `agent.tools`, work rounds run as stream-json sessions (`stream.go`): the CLI sends every permission request to the agent (`--permission-prompt-tool stdio`, a hidden flag), `Tools.decide` answers it, and `permissions.ask` rules make sure nothing runs without that. Never pass `--allowedTools`: the CLI would then decide calls itself. This rests on CLI behaviour verified on 2.1.284 and 2.1.285, which the live tests exercise: re-run them on every `CLAUDE_CODE_VERSION` bump with `ITAKEIT_LIVE=1 go test ./pkg/worker -run Live`.
- Fix mode: `Tools.decide` returns Ask for write entries, the session (`stream.go`) handles each request on its own goroutine and calls `Task.Approve`, which `pkg/agent/approval.go` implements on the event loop: approval messages keyed by their ts, resolved only by a reaction from `agent.approver`. Tool rounds hold an `Agent.sessions` token instead of `sem`, so a round waiting on an approval never blocks triage. The worker records what ran (`Result.Actions`) from the stream, not from the model; `footer` renders it.
- MCP servers (`agent.mcp_servers`) reach the CLI only through `dir/control/mcp.json` (`mcpFile`, 0600, re-checked before every session), never through its environment. The tools probe learns each server's tools from the init line; unlisted ones are passed as `--disallowedTools` to every later round.
- Memory (`memory.go`, `agent.memory`): `Memory` runs the `poma-memory` CLI with its own allow-listed environment (`memEnvNames`, no `POMA_*`). `Claude.recall` appends matches to the user message, never to the system prompt: they come from Slack threads. `Check` fails startup when the gate cannot be trusted, and in the base image (`ITAKEIT_AGENT_IMAGE`) names `Dockerfile.mem`. Learnings are saved by `pkg/agent/memory.go`: an approval request carries the learning, its task ts and `Memory.Sign` (HMAC, key in the memory dir), and is read back with `reactions.get`, so it needs no state and survives restarts. The signature is what tells it from a model reply of the same shape: keep checking it. `TestLiveMemory` runs the real CLI with `POMA_MEMORY_BIN` set.
- `Probe` runs once at startup. It fails fast on a broken login or model and learns the login email, which `scrub` then removes from everything posted.
- Tests: `claudecode_test.go` runs `testdata/fake-claude`, which records argv and stdin to files named by `FAKE_ARGS`/`FAKE_STDIN` and prints `FAKE_OUT`. Called with `--input-format stream-json` it plays the session script in `FAKE_SCRIPT` instead (`stream_test.go`, `fakeStream`). `cmd/itakeit-agent`'s Linux test only runs as a normal user, since root reads every environ. `api_test.go` points the SDK at an `httptest` server.

Prompt text (`triagePrompt`, `workPrompt`) is a security boundary. The Slack thread arrives in the user message and is untrusted. Knowledge is appended to the system prompt inside `<knowledge>` as trusted. Keep that split when editing prompts or adding inputs.
