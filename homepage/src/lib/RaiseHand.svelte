<script lang="ts">
  let { size = '1.25em' }: { size?: string } = $props()
</script>

<picture>
  <source srcset="./raise-hand.png" media="(prefers-reduced-motion: reduce)" />
  <img src="./raise-hand.gif" alt="🙋" width="64" height="64" style:height={size} style:width={size} />
</picture>

<style>
  img { vertical-align: -0.22em; display: inline; }
</style>
