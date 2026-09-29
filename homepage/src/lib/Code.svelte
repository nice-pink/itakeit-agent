<script lang="ts">
  let { code, label = '' }: { code: string; label?: string } = $props()
  let copied = $state(false)

  async function copy() {
    try {
      await navigator.clipboard.writeText(code)
      copied = true
      setTimeout(() => (copied = false), 1500)
    } catch {
      copied = false
    }
  }
</script>

<div class="code">
  <div class="bar">
    <span>{label}</span>
    <button type="button" onclick={copy} aria-live="polite">{copied ? 'Copied' : 'Copy'}</button>
  </div>
  <pre><code>{code}</code></pre>
</div>

<style>
  .code { border: 2px solid var(--ink); background: var(--code-bg); color: var(--code-fg); box-shadow: 4px 4px 0 var(--ink); margin: 0.75rem 0 1.25rem; min-width: 0; }
  .bar { display: flex; justify-content: space-between; align-items: center; padding: 0.35rem 0.6rem 0.35rem 0.9rem; border-bottom: 1px solid #ffffff22; font: 0.75rem/1 var(--mono); color: #b9c9c1; }
  button { font: 600 0.75rem/1 var(--mono); background: var(--green-dark); color: #fff; border: 0; padding: 0.4rem 0.6rem; cursor: pointer; }
  button:hover { background: var(--ink); }
  pre { margin: 0; padding: 0.9rem; overflow-x: auto; font: 0.85rem/1.55 var(--mono); }
</style>
