<script lang="ts">
  import RaiseHand from './RaiseHand.svelte'

  // Renders text with every 🙋 as the animated hand.
  let { text }: { text: string } = $props()
  let parts = $derived(text.split('🙋'))
</script>

{#each parts as part, i (i)}{#if i > 0}<RaiseHand />{/if}{part}{/each}
