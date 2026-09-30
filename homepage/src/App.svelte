<script lang="ts">
  import manifest from '../../slack-app-manifest.yaml?raw'
  import Code from './lib/Code.svelte'
  import Demo from './lib/Demo.svelte'
  import Emojify from './lib/Emojify.svelte'
  import { itakeit, repo } from './lib/site'

  const features = [
    { icon: '🙋', title: 'Claims like a teammate', text: 'After a short delay, so people get the first pick, it asks Claude whether its skills cover the task. If so, it reacts 🙋 and says in the thread what it will do.' },
    { icon: '🧵', title: 'Works in the thread', text: 'It replies in the task thread and sets ✅ done, ❓ needs info or ⛔ blocked. An answer to its question, or a mention, starts another round.' },
    { icon: '📚', title: 'Knows your docs', text: 'Runbooks, architecture notes and FAQs you list as knowledge go with every request, cached, so it answers from your facts rather than a guess.' },
    { icon: '🔍', title: 'Investigates with tools', text: 'Give it read-only commands and MCP tools, such as kubectl get or an issue tracker, and it looks before it answers, then proposes the fix.' },
    { icon: '✍️', title: 'Fixes, with your sign-off', text: 'In fix mode it can make the changes you allow. Each one is posted before it runs, and with an approver it runs only after their ✔️.' },
    { icon: '🔒', title: 'Decides every tool call', text: 'The Claude CLI runs nothing on its own: each call goes to the agent, which checks it against your allow-list. The Slack tokens never reach the CLI.' },
    { icon: '🧩', title: 'Plugs into itakeit', text: 'It never talks to itakeit directly. itakeit sees one more user, so ownership, pings and reminders work for the agent exactly as for people.' },
    { icon: '💤', title: 'No database', text: 'Its state is its own reactions in the channel. After a restart it picks up the tasks it was working on. Tasks posted while it was down are not triaged.' },
  ]

  const modes = [
    ['Tools', 'none', 'read entries', 'read and write entries'],
    ['Takes', 'questions, explanations, drafts, reviews', 'tasks where investigating helps', 'tasks its tools can complete'],
    ['Ends with', 'an answer, ✅', 'a proposal a human applies, ⛔', 'the change made, ✅'],
    ['Writes', 'never', 'never: write tools are not even shown', 'posted first; with an approver, run only after their ✔️'],
  ]

  const config = `# itakeit's config.yaml, plus:
agent:
  itakeit_app: A0123456789   # itakeit's App ID (api.slack.com/apps)
  claim_delay_seconds: 120   # let people pick first; unset is 0
  skills: |
    Answer questions about Go and our Kubernetes setup.
    Explain error messages and logs pasted into the task.
  knowledge:
    - knowledge/runbooks.md`

  const run = `docker run -d --name itakeit-agent --restart unless-stopped -e AGENT_SLACK_BOT_TOKEN -e AGENT_SLACK_APP_TOKEN -e CLAUDE_CODE_OAUTH_TOKEN -v "$PWD/config.yaml:/config/config.yaml:ro" -v "$PWD/knowledge:/config/knowledge:ro" ghcr.io/nice-pink/itakeit-agent:latest`

  const build = `git clone https://github.com/nice-pink/itakeit-agent.git && docker build -t itakeit-agent itakeit-agent`

  const tools = `agent:
  mode: fix                    # or propose: investigate and propose only
  approver: U0123456789        # every write waits for this user's ✔️
  tools:
    read:
      - "Bash(kubectl get *)"
      - "Bash(kubectl describe *)"
      - mcp__github__get_issue
    write:
      - "Bash(kubectl rollout restart *)"
  mcp_servers:
    github:
      type: stdio
      command: github-mcp-server
      args: [stdio]
      env: [GITHUB_PERSONAL_ACCESS_TOKEN]
  env: [KUBECONFIG]`

  const memory = `agent:
  approver: U0123456789   # saves each learning with their ✔️
  memory:
    enabled: true
    dir: memory           # next to config.yaml, writable
    results: 3            # notes added to a request, at most
    approval: true        # default; false saves unreviewed`

  const memoryRun = `docker run -d --name itakeit-agent --restart unless-stopped -e AGENT_SLACK_BOT_TOKEN -e AGENT_SLACK_APP_TOKEN -e CLAUDE_CODE_OAUTH_TOKEN -v "$PWD/config.yaml:/config/config.yaml:ro" -v "$PWD/knowledge:/config/knowledge:ro" -v itakeit-memory:/config/memory ghcr.io/nice-pink/itakeit-agent-mem:latest`

  const usage = [
    ['anyone', 'posts a task in the channel', 'after claim_delay_seconds (0 unless set; the example config uses 120), if nobody took it, the agent asks Claude whether its skills cover it'],
    ['agent', 'takes the task', 'reacts 🙋, says in the thread what it will do, reacts 🚧, works it, replies, then sets ✅, ❓ or ⛔'],
    ['someone', 'claims the task first', 'the agent leaves it, unless claim_owned: true lets it join as a co-owner'],
    ['reporter', 'answers the agent’s ❓', 'another round starts, with the whole thread'],
    ['anyone', 'replies on a ⛔ task, or mentions the agent on any task it owns', 'another round starts, even on a done task'],
    ['anyone', 'replies while a round runs', 'one more round runs after it, so the reply is read'],
    ['approver', 'reacts ✔️ or ❌ on an approval request', 'the change runs, or is denied and the agent says so. Nobody else’s reaction counts.'],
    ['approver', 'reacts ✔️ or ❌ on a learning request (with memory)', 'the learning is saved to memory, or dropped. Nobody else’s reaction counts.'],
    ['anyone', 'deletes the task message', 'the round stops and its pending approvals are cancelled'],
  ]
</script>

<header class="nav">
  <div class="wrap row">
    <a class="brand" href="#top"><img src="./turtle-parrot.png" alt="" /> itakeit-agent</a>
    <nav>
      <a href="#how">How it works</a>
      <a href="#setup">Setup</a>
      <a href="#tools">Tools</a>
      <a href="#memory">Memory</a>
      <a href="#usage">Usage</a>
      <a href={repo}>GitHub</a>
    </nav>
  </div>
</header>

<main id="top">
  <section class="hero wrap">
    <div class="pitch">
      <img class="turtle" src="./turtle-parrot.png" alt="pixel parrot riding the itakeit turtle" width="400" height="340" />
      <h1>A teammate in your Slack task channel. <span>It takes what it can do.</span></h1>
      <p class="lead"><b>itakeit-agent</b> watches the Slack channels your <a href={itakeit}>itakeit</a> bot tracks and claims the tasks its skills cover. It works them in the thread the way a person does, with <Emojify text="🙋" />, a reply and a status, on Claude. Give it tools and it investigates; allow changes and it makes them, with an approver only after they say so.</p>
      <div class="cta">
        <a class="btn" href="#setup">Set it up</a>
        <a class="btn ghost" href={repo}>View on GitHub</a>
      </div>
      <p class="small">Self-hosted. One Docker container, your Claude login, no database, no public URL.</p>
    </div>
    <div class="demo">
      <Demo />
      <p class="small center">Try it: pick a mode and step through a round.</p>
    </div>
  </section>

  <section id="how" class="band">
    <div class="wrap">
      <h2>How it works</h2>
      <p class="sub">It joins the channel as a separate Slack app and follows the same rules as everyone else.</p>
      <div class="grid">
        {#each features as f (f.title)}
          <article class="card">
            <div class="icon"><Emojify text={f.icon} /></div>
            <h3>{f.title}</h3>
            <p><Emojify text={f.text} /></p>
          </article>
        {/each}
      </div>

      <h3 class="modes-title">Three modes</h3>
      <p class="scroll-hint">Scroll the table sideways →</p>
      <div class="table">
        <table>
          <thead><tr><th></th><th>answer</th><th>propose</th><th>fix</th></tr></thead>
          <tbody>
            {#each modes as [label, ...cols] (label)}
              <tr><td>{label}</td>{#each cols as c, i (i)}<td>{c}</td>{/each}</tr>
            {/each}
          </tbody>
        </table>
      </div>
    </div>
  </section>

  <section id="setup" class="wrap setup">
    <h2>Setup</h2>
    <p class="sub">About fifteen minutes. You need a channel running <a href={itakeit}>itakeit</a>, a Claude login, and a machine that runs Docker.</p>

    <ol class="steps">
      <li>
        <h3>Create a second Slack app</h3>
        <p>The agent needs its own app: itakeit ignores reactions from its own bot. Open <a href="https://api.slack.com/apps">api.slack.com/apps</a>, choose <b>Create New App</b>, then <b>From an app manifest</b>, and paste this manifest.</p>
        <details>
          <summary>Show slack-app-manifest.yaml</summary>
          <Code code={manifest.trim()} label="slack-app-manifest.yaml" />
        </details>
        <p>It requests <code>channels:history</code>, <code>groups:history</code>, <code>channels:read</code>, <code>groups:read</code>, <code>chat:write</code>, <code>reactions:read</code> and <code>reactions:write</code>, and subscribes to reactions for approvals and to join, leave and archive events, so it follows the channels it is in. Socket Mode, so no request URL.</p>
      </li>
      <li>
        <h3>Get the two tokens</h3>
        <p>Under <b>Basic Information → App-Level Tokens</b>, generate a token with <code>connections:write</code>: that <code>xapp-…</code> token is <code>AGENT_SLACK_APP_TOKEN</code>. Under <b>Install App</b>, install the app and copy the <code>xoxb-…</code> Bot User OAuth Token: <code>AGENT_SLACK_BOT_TOKEN</code>.</p>
      </li>
      <li>
        <h3>Invite it to the channels</h3>
        <p>In each channel itakeit tracks, run <code>/invite @itakeit-agent</code>. The agent works in the same channels as itakeit, from the shared config: its <code>channels</code> list, or with <code>auto_channels: true</code> every channel the agent is invited to, so inviting it is all it takes to add one.</p>
      </li>
      <li>
        <h3>Add the agent block to config.yaml</h3>
        <p>Start from itakeit's own <code>config.yaml</code>, so both apps agree on the channels and the emoji, and add an <code>agent</code> block. itakeit ignores it, so both can mount the same file. <code>skills</code> is what the agent takes; <code>knowledge</code> files are optional. Every other setting is in <a href="{repo}/blob/main/config.example.yaml">config.example.yaml</a>.</p>
        <Code code={config} label="config.yaml" />
      </li>
      <li>
        <h3>Log in to Claude</h3>
        <p>On any machine with Claude Code, run <code>claude setup-token</code> once and keep the token as <code>CLAUDE_CODE_OAUTH_TOKEN</code>. Usage counts against that Claude plan. To bill the API instead, set <code>backend: api</code> and pass <code>-e ANTHROPIC_API_KEY</code> in place of the token; tools need the default <code>claude-code</code> backend.</p>
      </li>
      <li>
        <h3>Run the container</h3>
        <p>Export the three tokens, then start the published image from the directory with <code>config.yaml</code> and your knowledge files. It is built for amd64 and arm64; <code>latest</code> follows <code>main</code>, and releases are tagged.</p>
        <Code code={'export AGENT_SLACK_BOT_TOKEN=xoxb-... AGENT_SLACK_APP_TOKEN=xapp-... CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat01-...'} label="shell" />
        <Code code={run} label="shell" />
        <p>Or build it yourself, and run <code>itakeit-agent</code> in place of the image name above:</p>
        <Code code={build} label="shell" />
        <p>To run itakeit next to it, the repository has Docker Compose files for both: <code>docker-compose-remote.yml</code> with the published images, <code>docker-compose-local.yml</code> built from source.</p>
      </li>
      <li>
        <h3>Check it</h3>
        <p>The log shows <code>authenticated</code> and then <code>connected to slack</code>. A setup problem (channel, login, config) stops it at startup with an error that names the fix. Then post a question the skills cover and watch it take the task.</p>
      </li>
    </ol>

    <div class="note">
      <b>Anyone in the channel can talk to it.</b> Keep secrets out of the knowledge files, since the model can be made to quote them, and use a Claude login whose email you are fine showing in the channel: the agent scrubs it from replies, but a determined task can get it out in pieces.
    </div>
  </section>

  <section id="tools" class="band">
    <div class="wrap">
      <h2>Tools and approvals</h2>
      <p class="sub">Optional. Without tools the agent can only write. With them it can look, and in fix mode act, within what you list.</p>
      <div class="split">
        <div class="points">
          <p><b>You list every tool.</b> Read entries run freely; write entries run only in fix mode. Bash entries name the command, such as <code>Bash(kubectl get *)</code>, and a command with pipes, redirects or <code>;</code> matches nothing, so an entry cannot be stretched into a second command.</p>
          <p><b>Every call goes through the agent.</b> The Claude CLI asks the agent about each tool call and runs only what it allows. File tools stay in a per-task directory, and a session that runs a tool on its own is stopped.</p>
          <p><b>Changes are seen before they run.</b> Each write is posted in the thread first. With an <code>approver</code>, it waits for that person’s ✔️ on the approval message (30 minutes by default, then it is denied), and every reply lists the actions that ran. Fix mode without an approver needs <code>allow_unapproved_writes: true</code>, since anyone in the channel can then trigger the write entries.</p>
          <p><b>Secrets, as far as it goes.</b> The CLI gets an allow-listed environment without the Slack tokens, and MCP server secrets go only into a private config file. Known secret values are scrubbed from replies, best effort. What a read entry can read, the channel can get quoted: <code>Bash(kubectl get *)</code> reads Kubernetes Secrets too, so keep entries and credentials narrow.</p>
          <p><b>Bring your tools.</b> The image holds only the Claude CLI: which tools and credentials a deployment gets is your call. The repository's examples build on it: <a href="{repo}/tree/main/examples/kubectl">kubectl</a> with a mounted kubeconfig, <a href="{repo}/tree/main/examples/gcloud">gcloud</a> for GKE, and <a href="{repo}/tree/main/examples/in-cluster">in-cluster</a>, where kubectl uses the pod's ServiceAccount. A kubeconfig has to work inside the container: no paths or auth plugins from your machine. Other tools and MCP servers go into your own image <code>FROM</code> the agent's, installed as <code>USER root</code>, then back to <code>USER node</code>. Tools need <code>backend: claude-code</code>.</p>
          <p><b>Credentials set the limit.</b> Fix mode gives each session a fresh, empty home directory, so a write cannot plant anything for the next one; the agent copies the kubeconfig named in <code>KUBECONFIG</code> into it. <code>allow_real_home: true</code> keeps the real one instead, with an approver. Read entries that can print or change credentials, such as <code>Bash(kubectl *)</code>, <code>kubectl config</code> or <code>gcloud auth</code>, are refused at startup, and so is every call with a flag like <code>--insecure-skip-tls-verify</code> or <code>--kubeconfig</code>. Give the tools an identity that can do no more than the entries need.</p>
        </div>
        <Code code={tools} label="config.yaml (agent block)" />
      </div>
    </div>
  </section>

  <section id="memory" class="wrap setup">
    <h2>Memory</h2>
    <p class="sub">An advanced, additional feature. Off by default, and it runs only in the separate <code>itakeit-agent-mem</code> image. Everything above works without it.</p>
    <div class="split">
      <div class="points">
        <p><b>A separate image.</b> <code>Dockerfile.mem</code> builds the agent with <a href="https://github.com/poma-ai/poma-memory">poma-memory</a> and its small embedding model: local search, no API key, nothing downloaded at run time. The regular image refuses to start with memory enabled and names the image to run instead. It works with every mode and both backends.</p>
        <p><b>It looks before it works.</b> Before triage and every round, the agent searches its memory for the task and adds the notes that match to the request. They are marked as notes from earlier Slack threads: hints to check, never instructions, and kept apart from your trusted knowledge files.</p>
        <p><b>It learns from done tasks.</b> When a task ends ✅, the agent can propose what it learned. With approval, the default, it posts the learning in the thread and saves it only after the approver’s ✔️; ❌ drops it. Requests are signed, so a reply made to look like one saves nothing, and they still count after a restart.</p>
        <p><b>Plain Markdown.</b> Learnings go to <code>learnings/YYYY-MM.md</code> in the memory directory, next to any runbooks or notes you add there yourself. The search index is a SQLite file the agent rebuilds from the Markdown, so the Markdown is what to back up.</p>
        <p><b>Strict about relevance.</b> When nothing matches well, it adds nothing rather than the best bad match. At startup it checks that this filter works and refuses to start otherwise.</p>
      </div>
      <Code code={memory} label="config.yaml (agent block)" />
    </div>
    <p>Run the published memory image with a writable volume for the memory directory, or build it from <code>Dockerfile.mem</code>:</p>
    <Code code={memoryRun} label="shell" />
  </section>

  <section id="usage" class="wrap setup">
    <h2>Usage</h2>
    <p class="sub">What people do in the channel, and what the agent does in return.</p>
    <p class="scroll-hint">Scroll the table sideways →</p>
    <div class="table">
      <table>
        <thead><tr><th>Who</th><th>Does</th><th>Effect</th></tr></thead>
        <tbody>
          {#each usage as [who, does, effect] (does)}
            <tr><td>{who}</td><td>{does}</td><td><Emojify text={effect} /></td></tr>
          {/each}
        </tbody>
      </table>
    </div>
  </section>
</main>

<footer>
  <div class="wrap row">
    <span><img src="./turtle-parrot.png" alt="" /> itakeit-agent</span>
    <div>Needs <a href={itakeit}>itakeit</a>: task tracking in Slack threads, dead simple.</div>
    <div>built by <a href="https://nice.pink">nice-pink</a></div>
  </div>
</footer>

<style>
  .wrap { max-width: 1120px; margin: 0 auto; padding: 0 16px; }
  .row { display: flex; align-items: center; justify-content: space-between; gap: 1rem; flex-wrap: wrap; }
  .small { font-size: 0.85rem; color: var(--muted); }
  .center { text-align: center; }

  .nav { position: sticky; top: 0; z-index: 10; background: color-mix(in srgb, var(--bg) 92%, transparent); backdrop-filter: blur(6px); border-bottom: 2px solid var(--ink); }
  .nav .row { min-height: 60px; }
  .brand { display: flex; align-items: center; gap: 0.5rem; font: 700 1.15rem var(--mono); color: var(--ink); text-decoration: none; }
  .brand img, footer img { width: auto; height: 34px; image-rendering: pixelated; vertical-align: middle; }
  nav { display: flex; gap: 1.1rem; flex-wrap: wrap; }
  nav a { color: var(--ink); text-decoration: none; font-weight: 600; font-size: 0.95rem; }
  nav a:hover { color: var(--green); }

  .hero { display: grid; grid-template-columns: 1fr 1.05fr; gap: 3rem; align-items: center; padding-top: 3rem; padding-bottom: 4rem; }
  .turtle { width: 170px; height: auto; image-rendering: pixelated; margin: 0 0 1rem -8px; }
  h1 { font: 800 clamp(2.2rem, 5vw, 3.5rem)/1.05 var(--sans); letter-spacing: -0.03em; margin: 0 0 1rem; }
  h1 span { color: var(--green); display: block; }
  .lead { font-size: 1.15rem; color: var(--muted); margin: 0 0 1.5rem; max-width: 34rem; }
  .cta { display: flex; gap: 0.8rem; flex-wrap: wrap; margin-bottom: 0.8rem; }
  .btn { display: inline-block; padding: 0.75rem 1.3rem; font-weight: 700; text-decoration: none; background: var(--green-dark); color: #fff; border: 2px solid var(--ink); box-shadow: 4px 4px 0 var(--ink); transition: transform 0.1s, box-shadow 0.1s; }
  .btn:hover { transform: translate(-2px, -2px); box-shadow: 6px 6px 0 var(--ink); }
  .btn:active { transform: translate(2px, 2px); box-shadow: 2px 2px 0 var(--ink); }
  .btn.ghost { background: var(--panel); color: var(--ink); }
  .demo { min-width: 0; }

  h2 { font: 800 clamp(1.8rem, 4vw, 2.5rem)/1.1 var(--sans); letter-spacing: -0.02em; margin: 0 0 0.4rem; }
  .sub { color: var(--muted); margin: 0 0 2rem; }
  section { scroll-margin-top: 70px; }

  .band { background: var(--green-soft); border-top: 2px solid var(--ink); border-bottom: 2px solid var(--ink); padding: 4rem 0; }
  .grid { display: grid; grid-template-columns: repeat(4, minmax(0, 1fr)); gap: 1.2rem; }
  .card { background: var(--panel); border: 2px solid var(--ink); box-shadow: 4px 4px 0 var(--ink); padding: 1.2rem 1.3rem; }
  .card h3 { margin: 0.4rem 0 0.3rem; font-size: 1.1rem; }
  .card p { margin: 0; color: var(--muted); font-size: 0.95rem; }
  .icon { font-size: 1.7rem; }
  .modes-title { margin: 3rem 0 1rem; font-size: 1.4rem; }

  .setup { padding-top: 4rem; padding-bottom: 4rem; }
  .steps { list-style: none; counter-reset: step; padding: 0; margin: 0; max-width: 820px; }
  .steps > li { counter-increment: step; position: relative; padding: 0 0 1.5rem 3.6rem; border-left: 2px dashed var(--line); margin-left: 1.2rem; min-width: 0; }
  .steps > li:last-child { border-left-color: transparent; }
  .steps > li::before { content: counter(step); position: absolute; left: -1.25rem; top: -0.2rem; width: 2.5rem; height: 2.5rem; display: grid; place-items: center; font: 700 1.1rem var(--mono); background: var(--green); color: #fff; border: 2px solid var(--ink); box-shadow: 3px 3px 0 var(--ink); }
  .steps h3 { margin: 0 0 0.4rem; font-size: 1.2rem; }
  .steps p { margin: 0 0 0.6rem; }
  details { margin: 0.4rem 0 0.8rem; }
  summary { cursor: pointer; font-weight: 600; color: var(--green-dark); }
  .note { max-width: 820px; background: #fff3e6; border: 2px solid var(--ink); border-left: 8px solid var(--orange); padding: 1rem 1.2rem; }

  .split { display: grid; grid-template-columns: 1fr 1fr; gap: 2rem; align-items: start; }
  .points p { margin: 0 0 1rem; }
  .split > :global(.code) { margin-top: 0; }

  .table { overflow-x: auto; background: var(--panel); border: 2px solid var(--ink); box-shadow: 4px 4px 0 var(--ink); }
  table { width: 100%; border-collapse: collapse; font-size: 0.95rem; }
  th, td { text-align: left; padding: 0.65rem 0.9rem; border-bottom: 1px solid var(--line); vertical-align: top; }
  th { background: var(--ink); color: #fff; font-weight: 600; }
  td:first-child { font-family: var(--mono); font-size: 0.85rem; white-space: nowrap; color: var(--green-dark); }
  tr:last-child td { border-bottom: 0; }

  footer { border-top: 2px solid var(--ink); padding: 1.2rem 0; font-size: 0.9rem; }
  footer span { font: 700 1rem var(--mono); }

  @media (max-width: 1000px) {
    .grid { grid-template-columns: repeat(2, minmax(0, 1fr)); }
  }
  @media (max-width: 860px) {
    .hero, .split { grid-template-columns: 1fr; gap: 2rem; }
    .hero { padding-top: 1.5rem; }
    .turtle { width: 120px; }
    nav { gap: 0.8rem; }
    nav a { font-size: 0.85rem; }
  }
  .scroll-hint { display: none; margin: 0 0 0.4rem; font-size: 0.8rem; color: var(--muted); }
  @media (max-width: 520px) {
    .scroll-hint { display: block; }
    td:first-child { white-space: normal; }
    nav a:not(:last-child):not([href="#setup"]) { display: none; }
    .steps > li { padding-left: 2.2rem; margin-left: 1.2rem; }
    .grid { grid-template-columns: minmax(0, 1fr); }
  }
</style>
