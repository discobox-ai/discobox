<script lang="ts">
	import * as Card from '$lib/components/ui/card/index.js';
	import type { Sandbox } from './types';

	let {
		activeSandbox,
		lines
	}: {
		activeSandbox: Sandbox;
		lines: string[];
	} = $props();
</script>

<Card.Root
	class="flex min-h-0 flex-col overflow-hidden border-border bg-terminal-bg text-terminal-fg shadow-sm"
>
	<div
		class="flex h-9 shrink-0 items-center justify-between border-b border-border px-3 text-xs text-muted-foreground"
	>
		<div class="flex items-center gap-2">
			<span class="h-2 w-2 rounded-full bg-emerald-400"></span>
			<span class="font-medium uppercase tracking-[0.16em]">Terminal</span>
		</div>
		<span class="mono">{activeSandbox.id}@sandbox:/home/discobot/workspace/ui</span>
	</div>
	<div class="mono min-h-0 flex-1 overflow-auto p-4 text-[13px] leading-6 text-terminal-fg">
		{#each lines as line, index (index)}
			<div>{line}</div>
		{/each}
		<div><span class="text-muted-foreground">$</span> <span class="animate-pulse">▌</span></div>
	</div>
</Card.Root>
