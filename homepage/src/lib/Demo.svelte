<script lang="ts">
  import { tick } from 'svelte'
  import RaiseHand from './RaiseHand.svelte'
  type Mode = 'answer' | 'propose' | 'fix'
  type Msg = { who: 'agent' | 'itakeit' | 'tool'; text: string; code?: string }

  // One scripted round per mode. Each step adds the agent's next move, the way
  // the agent reports in the thread: claim, plan, work, reply, status.
  const scripts: Record<Mode, { task: string; steps: { status: string; msgs: Msg[] }[] }> = {
    answer: {
      task: 'What does "context deadline exceeded" from the payments client mean? Seeing it since this morning.',
      steps: [
        { status: '🙋', msgs: [{ who: 'agent', text: "I'm taking this. I'll explain the error and where to look first." }] },
        { status: '🚧', msgs: [] },
        {
          status: '✅',
          msgs: [{ who: 'agent', text: 'The client gave up waiting: the call ran longer than its context allowed. Per the payments runbook the timeout is 3 s. Check the p99 of the provider call; if it passed 3 s this morning, the provider is slow, not our code.' }],
        },
      ],
    },
    propose: {
      task: 'Pods in namespace web keep restarting since the last deploy.',
      steps: [
        { status: '🙋', msgs: [{ who: 'agent', text: "I'm taking this. I'll look at the pods and propose a fix." }] },
        { status: '🚧', msgs: [{ who: 'tool', text: 'kubectl get pods -n web' }, { who: 'tool', text: 'kubectl describe pod api-7f9c -n web' }] },
        {
          status: '⛔',
          msgs: [
            {
              who: 'agent',
              text: 'Proposal ready: a human needs to apply it. api-7f9c is OOMKilled at its 256Mi limit, and the deploy raised the cache size. Raise the limit:',
              code: 'kubectl set resources deploy/api -n web --limits=memory=512Mi',
            },
          ],
        },
      ],
    },
    fix: {
      task: 'I changed the api feature flags in the ConfigMap an hour ago, but api still serves the old ones.',
      steps: [
        { status: '🙋', msgs: [{ who: 'agent', text: "I'm taking this. I'll check whether the pods loaded the new config and restart them if not." }] },
        { status: '🚧', msgs: [{ who: 'tool', text: 'kubectl get configmap api-flags -n web' }, { who: 'tool', text: 'kubectl get pods -n web' }] },
        { status: '🚧', msgs: [] }, // the approval request, rendered below
      ],
    },
  }

  const modes: { key: Mode; label: string; note: string }[] = [
    { key: 'answer', label: 'answer', note: 'No tools: it answers from Claude and your knowledge files.' },
    { key: 'propose', label: 'propose', note: 'Read-only tools you list. It investigates, then proposes the change.' },
    { key: 'fix', label: 'fix', note: 'Write tools too. Each change is posted first; with an approver, it waits for their reaction.' },
  ]

  let mode = $state<Mode>('propose')
  let approveBtn = $state<HTMLButtonElement>()
  let againBtn = $state<HTMLButtonElement>()
  let stepBtn = $state<HTMLButtonElement>()
  let step = $state(0) // steps shown
  let approval = $state<'pending' | 'approved' | 'denied'>('pending')

  let script = $derived(scripts[mode])
  let shown = $derived(script.steps.slice(0, step))
  let status = $derived(approval === 'approved' ? '✅' : approval === 'denied' ? '⛔' : shown.at(-1)?.status ?? '')
  let asking = $derived(mode === 'fix' && step === script.steps.length)
  let done = $derived(mode === 'fix' ? approval !== 'pending' : step === script.steps.length)

  function pick(m: Mode) {
    mode = m
    step = 0
    approval = 'pending'
  }
  // Focus follows the round, so keyboard users land on what they can do next:
  // the approval buttons when they appear, "Run it again" when it ends.
  $effect(() => {
    if (asking && approval === 'pending') approveBtn?.focus()
  })
  $effect(() => {
    if (done) againBtn?.focus()
  })

  function next() {
    if (step < script.steps.length) step++
  }
  async function reset() {
    step = 0
    approval = 'pending'
    await tick()
    stepBtn?.focus() // "Run it again" is gone: keep the keyboard on the demo
  }
</script>

<div class="wrapper">
  <div class="tabs" role="group" aria-label="Agent mode">
    {#each modes as m (m.key)}
      <button type="button" aria-pressed={mode === m.key} class:on={mode === m.key} onclick={() => pick(m.key)}>{m.label}</button>
    {/each}
  </div>
  <p class="note">{modes.find((m) => m.key === mode)?.note}</p>

  <div class="slack" role="region" aria-label="Interactive example of the agent working a task in Slack">
    <div class="head"><span># itakeit</span><button type="button" class="reset" onclick={() => pick(mode)}>reset</button></div>

    <div class="msg">
      <div class="avatar a1">M</div>
      <div class="body">
        <div class="meta"><b>Maya</b> <span>10:42</span></div>
        <p>{script.task}</p>
        <div class="reactions">
          {#if step > 0}<span class="r on"><RaiseHand size="1.15em" /> <small>1</small></span>{/if}
          {#if status && status !== '🙋'}<span class="r on">{status} <small>1</small></span>{/if}
        </div>

        <div class="thread" aria-live="polite">
          <div class="msg">
            <div class="avatar bot"><img src="./turtle-parrot.png" alt="" /></div>
            <div class="body">
              <div class="meta"><b>itakeit</b> <span class="app">APP</span></div>
              <p><b>Status:</b> {status === '✅' ? '✅ done' : status === '⛔' ? '⛔ blocked' : status === '🚧' ? '🚧 in progress' : ''}{#if step > 0 && !['✅', '⛔', '🚧'].includes(status)}<RaiseHand /> claimed{:else if step === 0}⚪ unclaimed{/if}<br /><b>Owners:</b> {step > 0 ? '@itakeit-agent' : 'nobody yet'}</p>
            </div>
          </div>

          {#each shown as s, i (i)}
            {#each s.msgs as m, j (j)}
              {#if m.who === 'tool'}
                <p class="tool"><span>behind the scenes, allowed as a read entry:</span> <code>{m.text}</code></p>
              {:else}
                <div class="msg">
                  <div class="avatar agent"><img src="./turtle-parrot.png" alt="" /></div>
                  <div class="body">
                    <div class="meta"><b>itakeit-agent</b> <span class="app">APP</span></div>
                    <p>{m.text}</p>
                    {#if m.code}<pre>{m.code}</pre>{/if}
                  </div>
                </div>
              {/if}
            {/each}
          {/each}

          {#if asking}
            <div class="msg">
              <div class="avatar agent"><img src="./turtle-parrot.png" alt="" /></div>
              <div class="body">
                <div class="meta"><b>itakeit-agent</b> <span class="app">APP</span></div>
                <p><span class="mention">@you</span> approval needed (write 1 of max 20). Tool: <code>Bash</code></p>
                <pre>kubectl rollout restart deploy/api -n web</pre>
                <p class="legend">{approval === 'pending' ? 'React ✔️ to run it or ❌ to deny. Expires in 30 min.' : approval === 'approved' ? 'Approved by @you at 10:47 UTC' : 'Denied by @you'}</p>
                <div class="reactions">
                  <button type="button" bind:this={approveBtn} aria-label="approve" class:on={approval === 'approved'} disabled={approval !== 'pending'} onclick={() => (approval = 'approved')}>✔️ <small>{approval === 'approved' ? 2 : 1}</small></button>
                  <button type="button" aria-label="deny" class:on={approval === 'denied'} disabled={approval !== 'pending'} onclick={() => (approval = 'denied')}>❌ <small>{approval === 'denied' ? 2 : 1}</small></button>
                </div>
                {#if approval === 'pending'}<p class="small">You are the approver: click one. Nobody else’s reaction counts.</p>{/if}
              </div>
            </div>
            {#if approval !== 'pending'}
              <div class="msg">
                <div class="avatar agent"><img src="./turtle-parrot.png" alt="" /></div>
                <div class="body">
                  <div class="meta"><b>itakeit-agent</b> <span class="app">APP</span></div>
                  {#if approval === 'approved'}
                    <p>The pods started before the ConfigMap change, so they still had the old flags. Restarted deploy/api; the new pods serve the new flags.</p>
                    <p class="actions"><b>Actions taken:</b><br />• <code>Bash</code> <code>kubectl rollout restart deploy/api -n web</code>: approved by <span class="mention">@you</span>, ran</p>
                  {:else}
                    <p>The restart was not approved, so I changed nothing. To do it by hand:</p>
                    <pre>kubectl rollout restart deploy/api -n web</pre>
                  {/if}
                </div>
              </div>
            {/if}
          {/if}
        </div>
      </div>
    </div>

    <div class="foot">
      {#if !done && !asking}
        <button type="button" class="step" bind:this={stepBtn} onclick={next}>{step === 0 ? '▶ Let the agent take it' : '▶ Next step'}</button>
      {:else if done}
        <span class="small">Round finished. <button type="button" class="link" bind:this={againBtn} onclick={reset}>Run it again</button></span>
      {:else}
        <span class="small">Waiting for your reaction on the approval request above ↑</span>
      {/if}
    </div>
  </div>
</div>

<style>
  .tabs { display: flex; gap: 0.4rem; margin-bottom: 0.5rem; }
  .tabs button { font: 700 0.85rem var(--mono); padding: 0.35rem 0.8rem; border: 2px solid var(--ink); background: var(--panel); color: var(--ink); cursor: pointer; box-shadow: 3px 3px 0 var(--ink); }
  .tabs button.on { background: var(--green-dark); color: #fff; }
  .note { margin: 0 0 0.8rem; font-size: 0.85rem; color: var(--muted); min-height: 2.6em; }
  .slack { background: #fff; color: #1d1c1d; border: 2px solid var(--ink); box-shadow: 6px 6px 0 var(--ink); font-size: 0.92rem; text-align: left; min-width: 0; }
  .head { display: flex; justify-content: space-between; align-items: center; padding: 0.55rem 0.9rem; border-bottom: 1px solid #e3e3e3; font-weight: 700; }
  .reset { font: 0.75rem var(--mono); background: none; border: 1px solid #ccc; padding: 0.2rem 0.5rem; cursor: pointer; color: #555; }
  .msg { display: flex; gap: 0.6rem; padding: 0.7rem 0.9rem 0.2rem; }
  .body { min-width: 0; flex: 1; }
  .body p { margin: 0.15rem 0 0.35rem; }
  .avatar { flex: none; width: 36px; height: 36px; display: grid; place-items: center; font-weight: 700; color: #fff; }
  .a1 { background: #a84474; }
  .bot { background: #fff; border: 1px solid #e3e3e3; overflow: hidden; }
  .agent { background: var(--green-soft); border: 1px solid #b8dcc9; overflow: hidden; }
  .bot img, .agent img { width: 34px; height: 34px; object-fit: contain; }
  .meta { font-size: 0.85rem; }
  .meta span { color: #666; font-size: 0.75rem; margin-left: 0.25rem; }
  .meta .app { background: #eee; padding: 0 0.25rem; font-size: 0.65rem; font-weight: 700; }
  .reactions { display: flex; flex-wrap: wrap; gap: 0.3rem; margin: 0.3rem 0 0.4rem; min-height: 1.7rem; }
  .r, .reactions button { font-size: 0.9rem; border: 1px solid #ddd; background: #f4f4f4; border-radius: 999px; padding: 0.15rem 0.55rem; }
  .r.on { background: #e3f1ff; border-color: #1d9bd1; }
  .reactions button { cursor: pointer; transition: transform 0.1s; color: inherit; }
  .reactions button:hover:not(:disabled) { border-color: #1d9bd1; transform: translateY(-1px); }
  .reactions button.on { background: #e3f1ff; border-color: #1d9bd1; }
  .reactions button:disabled { cursor: default; }
  .reactions button:not(:disabled) { animation: nudge 1.6s ease-in-out infinite; }
  @keyframes nudge { 50% { box-shadow: 0 0 0 3px #1d9bd155; } }
  .reactions small { font-size: 0.75rem; color: #555; }
  .thread { border-left: 2px solid #e3e3e3; margin: 0.3rem 0 0.6rem; }
  .thread .msg { padding: 0.4rem 0.6rem 0.1rem; }
  .tool { margin: 0.2rem 0.6rem 0.2rem 3.4rem; font-size: 0.78rem; color: #616061; overflow-wrap: anywhere; }
  .tool code { background: #f4f4f4; padding: 0 0.25rem; }
  pre { margin: 0.2rem 0 0.4rem; padding: 0.45rem 0.6rem; background: #f8f8f8; border: 1px solid #e3e3e3; font: 0.8rem/1.45 var(--mono); white-space: pre-wrap; overflow-wrap: anywhere; }
  .legend { font-style: italic; color: #616061; font-size: 0.8rem; }
  .actions { font-size: 0.82rem; border-top: 1px dashed #ddd; padding-top: 0.3rem; }
  .actions code, .body p code { background: #f4f4f4; padding: 0 0.2rem; font-size: 0.8rem; }
  .mention { background: #e8f5fa; color: #1264a3; padding: 0 0.15rem; }
  .small { font-size: 0.8rem; color: #616061; }
  .foot { border-top: 1px dashed #ccc; padding: 0.6rem 0.9rem 0.7rem; background: #fbfaf5; min-height: 3.2rem; }
  .step { font: 700 0.85rem var(--sans); background: var(--green-dark); color: #fff; border: 2px solid var(--ink); padding: 0.4rem 0.8rem; cursor: pointer; box-shadow: 3px 3px 0 var(--ink); }
  .link { background: none; border: 0; padding: 0; color: var(--green-dark); text-decoration: underline; cursor: pointer; font: inherit; }
</style>
