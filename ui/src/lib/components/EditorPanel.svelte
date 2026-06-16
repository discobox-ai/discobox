<script lang="ts">
	import { Badge } from '$lib/components/ui/badge/index.js';
	import * as Card from '$lib/components/ui/card/index.js';
	import type { WorkspaceFile } from './types';

	let {
		files,
		codeLines
	}: {
		files: WorkspaceFile[];
		codeLines: string[];
	} = $props();
</script>

<Card.Root
	class="grid min-h-0 grid-cols-1 overflow-hidden shadow-sm lg:grid-cols-[240px_minmax(0,1fr)]"
>
	<div class="hidden min-h-0 border-r border-border bg-background lg:block">
		<div
			class="border-b border-border px-3 py-2 text-xs font-semibold uppercase tracking-[0.16em] text-muted-foreground"
		>
			Explorer
		</div>
		<div class="space-y-1 p-2 text-sm">
			{#each files as file (file.name)}
				<div class="flex items-center justify-between rounded-md px-2 py-1.5 hover:bg-tree-hover">
					<span class="truncate">{file.name}</span>
					<Badge class="mono h-4 px-1.5 text-[10px]">{file.state}</Badge>
				</div>
			{/each}
		</div>
	</div>

	<div class="flex min-h-0 min-w-0 flex-col">
		<div
			class="flex h-9 shrink-0 items-center justify-between border-b border-border bg-muted/30 text-xs"
		>
			<div class="flex h-full items-center border-r border-border bg-card px-3 font-medium">
				sandbox.go
			</div>
			<div class="px-3 text-muted-foreground">read-only mock editor</div>
		</div>
		<div class="mono min-h-0 flex-1 overflow-auto bg-card p-4 text-[13px] leading-6">
			{#each codeLines as line, index (index)}
				<div class="grid grid-cols-[3rem_minmax(0,1fr)] gap-3">
					<span class="select-none text-right text-muted-foreground">{index + 1}</span>
					<code class="whitespace-pre text-card-foreground">{line}</code>
				</div>
			{/each}
		</div>
	</div>
</Card.Root>
