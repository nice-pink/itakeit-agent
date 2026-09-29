# itakeit-agent

An agent for an [itakeit](https://github.com/nice-pink/itakeit) channel. It claims the tasks it can handle and reports its progress the way a person does: with reactions on the task message and replies in the task thread. It never talks to itakeit directly, so the channel shows the agent as the owner and every itakeit rule applies to it unchanged.

It runs on Claude, through the Claude Code CLI and its login (default) or through the Messages API with API credentials. Without tools it takes only tasks that a written reply completes: questions, explanations, drafts, reviews of pasted text. With `tools` (claude-code backend only) it can also investigate, with read-only commands you list, and propose what a human should change (see Tools). What it takes is set by `skills`, a short description of what it can do. What it knows beyond Claude's own training comes from `knowledge`, a list of Markdown files you write, such as runbooks, architecture notes, naming conventions and FAQs, sent with every triage and work request.

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

## Tools

With `agent.tools`, work rounds can investigate before they answer, and in fix mode make changes. It relies on Claude Code CLI behaviour verified on the version the image pins (`CLAUDE_CODE_VERSION`), which `ITAKEIT_LIVE=1 go test ./pkg/worker -run Live` checks again.

- `mode: propose` (the default): the agent reads with the read entries and proposes what a human should change. Write entries are never shown to the model.
- `mode: fix`: the agent also runs write entries to complete the task. Each write is posted in the task thread before it runs. With `approver`, it runs only after that user reacts to it. Without one, fix mode needs `allow_unapproved_writes: true`, since anyone in the channel can then trigger the write entries.

```yaml
agent:
  mode: fix
  approver: U0123456789        # member ID; every write waits for this user's reaction
  tools:
    read:
      - Read                   # files in the task's directory
      - Grep
      - "Bash(kubectl get *)"  # the command, or its first words and " *"
      - "Bash(kubectl version)"
    write:
      - Edit                   # files in the task's directory
      - "Bash(kubectl rollout restart *)"
  approval_emoji:              # default heavy_check_mark and x; must not be itakeit status emoji
    approve: heavy_check_mark
    deny: x
  approval_timeout_minutes: 30 # 1 to 40, the longest the CLI was verified to wait
  work_timeout_minutes: 15     # working time per round, 1 to 120; approval waits do not count
  max_sessions: 2              # tool rounds at once, running or waiting; default max_parallel
  env: [KUBECONFIG]            # what the tools need from the agent's environment
```

Approvals:

- The approval request names the approver, shows the exact command (for Bash: the command and timeout, never the model's own description of it) and offers the two emoji. Only a reaction by the approver's member ID, with one of the two emoji, on that message counts. Other people's reactions and thread replies do nothing. The message is edited to show the outcome: approved (by whom, when), denied, timed out, cancelled, or expired.
- Read and write entries must not overlap: a read entry that also covers a write entry (`Bash(kubectl *)` beside `Bash(kubectl rollout restart *)`) is refused, since the write would run as a read. The agent also checks write entries first.
- A write that is denied, times out, or whose request cannot be posted does not run. The model is told why. A lost reaction event is caught at the timeout, when the agent reads the message's reactions: holding both emoji denies.
- At most 20 writes per round; input over 8,000 bytes is refused unreviewed. The reply ends with an "Actions taken" list built from what actually ran, on error rounds too.
- Rounds waiting for an approval hold a `max_sessions` slot but no triage slot, so triage goes on while they wait. Other tool rounds do wait for a free session: with `max_sessions: 2`, two rounds waiting for approvals hold up every other task's work for as long as those approvals take. Raise `max_sessions` if approvals are slow.
- Deleting the task cancels its round and its pending approvals.
- When the agent restarts during a fix-mode round, writes may already have run. It marks that round's open approvals expired, asks in the thread whether to resume, and sets ❓. Only the approver's reply resumes the task (the reporter's when there is no approver). The reply is in the transcript, so "no, leave it" ends the round without changes. The question survives further restarts, and a reply the approver posted while the agent was down resumes the task when it starts. A task reported by a bot or an integration, with no approver set, has nobody who can answer, so after a restart it goes ⛔ to a human.
- Plain `Bash` and write entries that can run anything (shells, `sed -i`, `tee`, `cp` and the rest of the read-entry refusals) are accepted only with an approver.
- Fix mode runs every tool session with a fresh, empty `HOME`, because the CLI sources a snapshot of the shell rc file before every Bash command: one write to `~/.bashrc` would otherwise turn later read entries into arbitrary code. A `claude auth login` login lives in `HOME`, so fix mode needs `CLAUDE_CODE_OAUTH_TOKEN` from `claude setup-token`; the startup check says so if it is missing. Tool credentials normally under `HOME` (`~/.kube/config`, `~/.aws`) must be passed by path through `agent.env` (`KUBECONFIG`, `AWS_CONFIG_FILE`).
- Approvals need the `reaction_added` bot event, which `slack-app-manifest.yaml` now subscribes to. An app created from an earlier manifest needs it added and the app reinstalled.

MCP servers:

```yaml
agent:
  mcp_servers:
    github:                    # lowercase letters, digits, dashes
      type: stdio
      command: github-mcp-server
      args: [stdio]
      env: [GITHUB_PERSONAL_ACCESS_TOKEN]  # names of the agent's variables
    docs:
      type: http
      url: https://mcp.example.com/mcp     # https only
      headers:
        Authorization: DOCS_AUTH           # header: variable holding its full value
  tools:
    read: [mcp__github__get_issue, mcp__docs__search]
    write: [mcp__github__create_issue]
```

- Entries name single MCP tools, `mcp__<server>__<tool>`. Which of a server's tools only read is your call: the agent cannot see what a tool does, so a tool listed as read runs without approval.
- The variables a server names are read from the agent's environment at startup (unset fails startup) and written with the server into `mcp.json`, mode 0600, outside every task directory. They never enter the CLI's environment, so a Bash tool cannot print them: a server may therefore not name a variable the CLI gets anyway (`agent.env`, `CLAUDE_CODE_*`, `DISABLE_*`, proxies and the rest of the list above). Values of 8 characters or more are removed from everything posted, which also removes a harmless value such as `github.com` wherever it appears: keep non-secrets out of server variables, or put them in `args`. `mcp.json` is checked before every session and written again (atomically, never through a symlink) if anything changed it. A Bash read entry that can read files (`Bash(cat *)`) can still read `mcp.json`, and the server's own `/proc/<pid>/environ`.
- A stdio server runs in the task's directory, which rounds can write to: `command` must be absolute or a name on `PATH` (keep `.` and empty entries out of the agent's `PATH`, or a bare name resolves in the task directory too), and a relative path in `args` resolves inside the task directory. In fix mode the server also gets the round's fresh, empty `HOME`, so a server that keeps its config under `~` needs it passed another way (a variable, or an absolute path in `args`). An http server's `url` must be https with a host and no credentials in it; send credentials as headers.
- At startup the probe loads the servers and learns their tools; every server must be connected then, or startup fails. Every tool no entry names is disallowed for all later rounds, as are the CLI's generic MCP resource tools and, in propose mode, the MCP write entries: the model never sees them. A tool a server adds later shows up for one round, where calling it is denied, and is disallowed from the next. A server that is not connected in a round is logged, and its tools are missing that round.

Rules for every tool round:

- Every tool call goes to the agent for a decision. The CLI runs nothing on its own: each exposed tool has an `ask` rule, and nothing is passed as `--allowedTools`. The agent allows a call when it matches a read entry, asks the approver (or posts a notice) when it matches a write entry in fix mode, and denies everything else with a reason the model sees.
- A Bash command matches `Bash(<prefix> *)` when it is the prefix or starts with the prefix and a space, and `Bash(<command>)` only when equal. A command with `;`, `&`, `|`, `<`, `>`, `$`, backticks, a backslash, parentheses, braces, `#` or a newline matches nothing, so a read entry cannot be stretched into a second command. `run_in_background` and other Bash options are refused.
- Read entries refuse `Edit`, `Write`, `NotebookEdit`, plain `Bash`, a leading variable assignment, and Bash commands known to run others, write files or print the environment: shells, `env`, `printenv`, `xargs`, `sudo`, `find`, `git`, `sed`, `awk`, `tar`, `curl`, interpreters and package managers, `make`, `tee`, `cp`, `mv`, `rm`, editors and pagers, `docker` and similar. That list is best effort, and your entries are the real guarantee: an entry of one word, such as `Bash(kubectl *)`, reaches every subcommand (`kubectl exec`), and the agent warns about it at startup. Tools other than `Read`, `Grep`, `Glob`, `Edit`, `Write`, `NotebookEdit`, `Bash` and single MCP tools (`mcp__<server>__<tool>`) are refused. Anything a read entry can read, the channel can get quoted, so keep entries narrow: `Bash(kubectl get *)` also reads secrets.
- File tools are confined to the task's directory by the CLI (`--restricted`, verified for Read), and the agent checks their paths too: it resolves symlinks and refuses paths starting with `~` or with surrounding spaces, and absolute or `..` Glob patterns. Each task gets its own directory, removed when the agent sets the task done or the task is deleted. A human marking it done is not seen, so the directory then stays until the agent exits. A Bash read entry is not confined: `Bash(cat *)` reads any file the agent's user can, including the CLI's own environment in `/proc`, which holds the Claude login token.
- The values the CLI gets that a tool could print (every `CLAUDE_CODE_*` variable, which includes the OAuth token, the `agent.env` values of 8 characters or more, and the Slack tokens) are removed from everything posted, like the login email. That is best effort, as for the email: a value printed in pieces or encoded gets through. Tool sessions also set `CLAUDE_CODE_SUBPROCESS_ENV_SCRUB`, which makes the CLI strip credential variables from the Bash tool's shell: among them `CLAUDE_CODE_OAUTH_TOKEN`, `AWS_SECRET_ACCESS_KEY`, `AWS_SESSION_TOKEN`, `GOOGLE_APPLICATION_CREDENTIALS`, `CLOUDSDK_AUTH_ACCESS_TOKEN` and `VAULT_TOKEN` (read from the 2.1.284 binary; the live test confirmed a `*_TOKEN` variable is gone from the shell). A read entry such as `Bash(aws … *)` therefore does not see those even when `agent.env` passes them: give such tools file-based credentials instead (`AWS_CONFIG_FILE`, `AWS_SHARED_CREDENTIALS_FILE`, `CLOUDSDK_CONFIG`, `KUBECONFIG`).
- At startup the agent checks that permission requests reach it: one extra model call that asks for `echo probe` and denies it. A session is stopped if the CLI starts with another permission mode or other tools than listed, or if a tool ran without the agent's decision.
- A work round that runs out of time is stopped with every process it started, including the Bash tool's detached shell: SIGTERM to the whole tree, SIGKILL to the CLI after 5 s, and processes left behind are killed and reaped by the agent (Linux).
- On Linux the agent makes its own `/proc/<pid>/environ` unreadable to its children, so a tool cannot read the Slack tokens there. The image therefore has no init process such as tini, which as PID 1 would hold the tokens in a readable `/proc/1/environ`; the agent is PID 1 and reaps what tool sessions leave behind itself (outside the image it makes itself a subreaper). On macOS, where `ps` shows another process's environment to the same user, run agents with tools in the image.
- The prompts tell the model that tool output is data, not instructions. In propose mode a proposal ends ⛔ with "Proposal ready: a human needs to apply it." Done is only for tasks that ask for no change. In fix mode done means the changes completed the task.

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

With `backend: claude-code`, each call is one `claude -p` run. The thread goes in on stdin and the answer comes back as `structured_output`. Each run gets no tools (`--tools ""`, except work rounds with `agent.tools`, see Tools), no MCP servers (`--strict-mcp-config`), no saved session, and no user or project settings or CLAUDE.md (`--setting-sources ""`), so none of the machine's hooks, plugins or instructions reach it. Managed (policy) settings still apply. The CLI gets only an allow-listed environment: `PATH`, `HOME`, `USER`, `LOGNAME`, `LANG`, `TMPDIR`, `CLAUDE_CONFIG_DIR`, `ANTHROPIC_BASE_URL`, every `CLAUDE_CODE_*` variable (including `CLAUDE_CODE_OAUTH_TOKEN`), every `DISABLE_*` variable (such as `DISABLE_TELEMETRY`), the proxy variables in both cases, `NODE_EXTRA_CA_CERTS`, `SSL_CERT_FILE` and `SSL_CERT_DIR`. The agent's Slack tokens and every other variable not listed here never reach it. Name further variables in `agent.env`. Earlier versions passed the whole environment, so a setup that relied on anything else now has to list it, most often Bedrock or Vertex credentials (`AWS_*`, `GOOGLE_APPLICATION_CREDENTIALS`, `GOOGLE_CLOUD_PROJECT`, `CLOUD_ML_REGION`, `ANTHROPIC_VERTEX_*`, `ANTHROPIC_BEDROCK_BASE_URL`), gateway settings (`ANTHROPIC_CUSTOM_HEADERS`, `API_TIMEOUT_MS`), `NODE_OPTIONS`, `NODE_USE_SYSTEM_CA`, `XDG_CONFIG_HOME`, `CLAUDE_TMPDIR` and `OTEL_*`. A missing one shows up as a failed startup probe. `ANTHROPIC_API_KEY` and `ANTHROPIC_AUTH_TOKEN` are never passed, because the CLI would use them instead of its login and bill the API, and `agent.env` refuses them and `AGENT_SLACK_*`. A run that takes longer than 15 minutes (a tool round: `work_timeout_minutes`) is killed: a work round then ends ⛔, a triage leaves the task alone.

The CLI adds an environment block to the prompt that the agent cannot turn off: the login's email, the working directory and the OS. The prompts tell the model not to reveal it, and verbatim copies of the email are removed from every reply, triage reason and error before posting. The agent learns the email from `claude auth status` and from the startup probe above, which asks the model what email its prompt carries. That is the only source for a `CLAUDE_CODE_OAUTH_TOKEN` login, so if the model declines to say, nothing is removed and the agent logs a warning at startup. This is best effort: a task can still talk the model into posting the email in pieces (a review got `"rh"`, `"@"` and the domain as separate strings), or into posting the OS or the temp directory path. Use a login whose email you are fine exposing to the channel.

With `backend: api`, requests stream from the Messages API with adaptive thinking. Server-side refusal fallbacks are on (`fallbacks: "default"`), so a request the model declines on policy grounds is retried on another model inside the same call. A work round that is still refused ends the task as ⛔ blocked. A refused triage leaves the task alone. Triage and work without tools share `max_parallel` with either backend; tool rounds use `max_sessions`.
