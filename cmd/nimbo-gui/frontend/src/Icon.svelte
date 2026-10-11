<script lang="ts" module>
  // Small line icons drawn in the text colour (currentColor), so they follow
  // light/dark mode and the accent wherever they sit. Add a shape to PATHS to
  // grow the set; emoji render differently on every Windows build and can't be
  // themed, which is why the folder picker uses these instead.
  const PATHS = {
    folder: "M2.5 4.5a1 1 0 0 1 1-1h3l1.5 1.5h4.5a1 1 0 0 1 1 1v6a1 1 0 0 1-1 1h-9a1 1 0 0 1-1-1z",
    chevron: "M6 3.5 10.5 8 6 12.5",
  } as const;
  export type IconName = keyof typeof PATHS;
</script>

<script lang="ts">
  let { name, size = 16 }: { name: IconName; size?: number } = $props();
</script>

<svg class="icon" width={size} height={size} viewBox="0 0 16 16" aria-hidden="true"
     fill="none" stroke="currentColor" stroke-width="1.3" stroke-linecap="round" stroke-linejoin="round">
  <path d={PATHS[name]} />
</svg>

<style>
  .icon { flex: 0 0 auto; display: block; }
</style>
