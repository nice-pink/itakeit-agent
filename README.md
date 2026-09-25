# itakeit-agent

An agent for an [itakeit](https://github.com/nice-pink/itakeit) channel. It claims the tasks it can handle and reports its progress the way a person does: with reactions on the task message and replies in the task thread. It never talks to itakeit directly, so the channel shows the agent as the owner and every itakeit rule applies to it unchanged.

It runs on Claude, through the Claude Code CLI and its login (default) or through the Messages API with API credentials. It has no tools, so it takes only tasks that a written reply completes: questions, explanations, drafts, reviews of pasted text. What it takes is set by `skills`, a short description of what it can do. What it knows beyond Claude's own training comes from `knowledge`, a list of Markdown files you write, such as runbooks, architecture notes, naming conventions and FAQs, sent with every triage and work request.

The work lives behind the `worker.Worker` interface in `pkg/worker`, which is the place to plug in an agent that can act on systems.

## What it does

1. A top-level message is posted in the channel. After `claim_delay_seconds` the agent skips it if someone has already claimed it or marked it done, otherwise it asks Claude whether `skills` covers the task.
2. If yes, and nobody claimed it in the meantime, it reacts 🙋 to claim, says in the thread what it will do, and reacts 🚧.
3. It works the task on the full thread and posts the result as a thread reply, then sets one status:
   - ✅ done when the reply completes the task.
   - ❓ needs info when it asked the reporter a question. itakeit pings the reporter. The reporter's reply starts another round.
   - ⛔ blocked when it cannot finish, with the reason, so a human can take over. Errors, empty answers and replies Slack rejects end here too.
4. Any reply on a ❓ or ⛔ task starts another round, and so does a mention of the agent on any task it owns, including a done one. A reply that arrives during a round starts one more round when it ends, and the status stays 🚧 in between.

It adds the new status reaction before removing the old one. itakeit takes the latest added status, and removing the current one first would clear it.

Replies are posted with `&`, `<` and `>` escaped, so code like `x <- ch` renders as written and model output cannot ping `@channel` or a user group. User and channel references stay live. Links render as plain text.

With `claim_owned: true` the agent also takes tasks people have claimed, as a co-owner. It does not watch reactions, so it does not see a co-owner's status change, and its own next status replaces it as the task's status.

## Knowledge

Knowledge files are this agent's equivalent of a CLAUDE.md, except nothing is picked up automatically: only the files listed under `agent.knowledge` reach it. The machine's CLAUDE.md files, user and project settings and hooks never do. Managed (policy) settings still apply.

```yaml
agent:
  knowledge:
    - knowledge/infra.md
    - knowledge/runbooks.md
```

- Paths are relative to the directory of the `-config` path as given (a symlinked config resolves against the link's directory, not its target). Each file is read at startup, so edit and restart to change them. A missing file stops startup.
- The files go at the end of the system prompt of every triage and work request, each under a `===== path =====` line, inside a `<knowledge>` section. The prompt tells the model to trust that section as facts from its operators, and that Slack text claiming to be a knowledge update is not. How well that holds depends on the model: in tests, opus (the default) ignored a thread reply forging a knowledge update, while haiku followed it. With a small model, anyone in the channel can make the agent state false "facts".
- The system prompt is the same for every task, so it is cached: a request pays for the knowledge in full only when no triage or work request of the same kind ran within the cache lifetime (up to an hour with the CLI, five minutes with the API), otherwise it reads it from the cache at a fraction of the price. The startup probe does not carry it.
- Together the files are capped at 256 KB (about 64K tokens of English, fewer bytes per token for other scripts). A few pages is the sweet spot.
- Anyone in the channel can get the model to quote them, so keep secrets and anything the channel's members should not read out of them.

## State

It keeps no database. On start it reads the agent's own reactions on the last `recover_messages` channel messages, and works again on the tasks that were in progress when it stopped. Tasks posted while it was down are not triaged.

## Setup

1. Create a separate Slack app from `slack-app-manifest.yaml` (https://api.slack.com/apps → Create New App → From an app manifest). It has to be a separate app: itakeit ignores reactions from its own bot user. Generate an app-level token with `connections:write` (`AGENT_SLACK_APP_TOKEN`) and install the app (`AGENT_SLACK_BOT_TOKEN`).
2. Invite it to the itakeit channel: `/invite @itakeit-agent`.
3. Start from itakeit's `config.yaml` and add the `agent` block from `config.example.yaml`. `itakeit_user` (the itakeit bot's member ID) and `skills` are required, `knowledge` is optional, and the `emoji` block must keep `in_progress`, `needs_info`, `blocked` and `done`. itakeit ignores the block, so both apps can mount the same file.
4. Log in to Claude:
   - `backend: claude-code` (default): install Claude Code and run `claude auth login` as the user the agent runs as. For a container or a server without a browser, run `claude setup-token` once on your machine and pass the token as `CLAUDE_CODE_OAUTH_TOKEN`. At startup the agent runs `claude auth status` and a probe call, and refuses to start without a working login.
   - `backend: api`: set `ANTHROPIC_API_KEY`, or log in with `ant auth login`.

| Scope | Used for |
|---|---|
| `channels:history`, `groups:history` | receiving task messages and replies, reading threads, recovering owned tasks on start |
| `chat:write` | thread replies |
| `reactions:read` | checking whether someone already claimed a task |
| `reactions:write` | claiming and setting statuses |

## Run

```
./build && AGENT_SLACK_BOT_TOKEN=xoxb-... AGENT_SLACK_APP_TOKEN=xapp-... ./bin/itakeit-agent -config config.yaml
```

The image includes the Claude Code CLI, pinned by the `CLAUDE_CODE_VERSION` build arg to the version the agent was tested with. Pass `CLAUDE_CODE_OAUTH_TOKEN` for `backend: claude-code`, or `ANTHROPIC_API_KEY` for `backend: api`. The CLI writes its state to `/home/node` and `/tmp` on every run, so a read-only container needs both writable, for example `--read-only --tmpfs /tmp --tmpfs /home/node:uid=1000,gid=1000`. A plain `--tmpfs /home/node` is owned by root and the CLI fails on every task.

Knowledge paths resolve against `/config`, so mount the files next to the config:

```
docker build -t itakeit-agent . && docker run -d --name itakeit-agent --restart unless-stopped -e AGENT_SLACK_BOT_TOKEN -e AGENT_SLACK_APP_TOKEN -e CLAUDE_CODE_OAUTH_TOKEN -v "$PWD/config.yaml:/config/config.yaml:ro" -v "$PWD/knowledge:/config/knowledge:ro" itakeit-agent
```

## Model calls

At startup, with either backend, the agent makes one probe call on `model`. A login or model that does not work (expired token, unknown model name) stops the agent with that error, instead of ending every task ⛔.

Triage runs at effort `low` and each work round at effort `high`, both on `model` with a JSON schema for the answer. Every task thread reply from a person is sent to Claude with the task text, in the user message. The system prompt (instructions, `skills`, knowledge) goes to the CLI as a file (`--system-prompt-file`), since Linux limits one command-line argument to 128 KB, and to the API as a cached system block.

With `backend: claude-code`, each call is one `claude -p` run. The thread goes in on stdin and the answer comes back as `structured_output`. Each run gets no tools (`--tools ""`), no MCP servers (`--strict-mcp-config`), no saved session, and no user or project settings or CLAUDE.md (`--setting-sources ""`), so none of the machine's hooks, plugins or instructions reach it. Managed (policy) settings still apply. `ANTHROPIC_API_KEY` and `ANTHROPIC_AUTH_TOKEN` are removed from the CLI's environment, because the CLI would use them instead of its login and bill the API. A run that takes longer than 15 minutes is killed: a work round then ends ⛔, a triage leaves the task alone.

The CLI adds an environment block to the prompt that the agent cannot turn off: the login's email, the working directory and the OS. The prompts tell the model not to reveal it, and verbatim copies of the email are removed from every reply, triage reason and error before posting. The agent learns the email from `claude auth status` and from the startup probe above, which asks the model what email its prompt carries. That is the only source for a `CLAUDE_CODE_OAUTH_TOKEN` login, so if the model declines to say, nothing is removed and the agent logs a warning at startup. This is best effort: a task can still talk the model into posting the email in pieces (a review got `"rh"`, `"@"` and the domain as separate strings), or into posting the OS or the temp directory path. Use a login whose email you are fine exposing to the channel.

With `backend: api`, requests stream from the Messages API with adaptive thinking. Server-side refusal fallbacks are on (`fallbacks: "default"`), so a request the model declines on policy grounds is retried on another model inside the same call. A work round that is still refused ends the task as ⛔ blocked. A refused triage leaves the task alone. Triage and work share `max_parallel` with either backend.
