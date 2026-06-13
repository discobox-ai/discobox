<script lang="ts">
	import type { Sandbox } from './types';

	let {
		activeSandbox,
		lines
	}: {
		activeSandbox: Sandbox;
		lines: string[];
	} = $props();
</script>

<section class="card card-border flex min-h-0 flex-col overflow-hidden bg-neutral shadow-sm">
	<div
		class="flex h-9 shrink-0 items-center justify-between border-b border-neutral-content/10 px-3 text-xs text-neutral-content/70"
	>
		<div class="flex items-center gap-2">
			<span class="h-2 w-2 rounded-full bg-success"></span>
			<span class="font-medium uppercase tracking-[0.16em]">Terminal</span>
		</div>
		<span class="mono">{activeSandbox.id}@sandbox:/home/discobot/workspace/ui</span>
	</div>
	<div class="mono min-h-0 flex-1 overflow-auto p-4 text-[13px] leading-6 text-success">
		{#each lines as line, index (index)}
			<div>{line}</div>
		{/each}
		<div><span class="text-neutral-content/60">$</span> <span class="animate-pulse">▌</span></div>
	</div>
</section>
