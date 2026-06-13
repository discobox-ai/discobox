<script lang="ts">
	import type { WorkspaceFile } from './types';

	let {
		files,
		codeLines
	}: {
		files: WorkspaceFile[];
		codeLines: string[];
	} = $props();
</script>

<section
	class="card card-border grid min-h-0 grid-cols-[240px_minmax(0,1fr)] overflow-hidden bg-base-100 shadow-sm"
>
	<div class="min-h-0 border-r border-base-300 bg-base-200/60">
		<div
			class="border-b border-base-300 px-3 py-2 text-xs font-semibold uppercase tracking-[0.16em] text-base-content/60"
		>
			Explorer
		</div>
		<div class="space-y-1 p-2 text-sm">
			{#each files as file (file.name)}
				<div class="flex items-center justify-between rounded-md px-2 py-1.5 hover:bg-base-300">
					<span class="truncate">{file.name}</span>
					<span class="badge badge-primary badge-soft badge-xs mono">{file.state}</span>
				</div>
			{/each}
		</div>
	</div>

	<div class="flex min-h-0 min-w-0 flex-col">
		<div class="flex h-9 shrink-0 items-center border-b border-base-300 bg-base-200/40 text-xs">
			<div class="flex h-full items-center border-r border-base-300 bg-base-100 px-3 font-medium">
				sandbox.go
			</div>
			<div class="px-3 text-base-content/60">read-only mock editor</div>
		</div>
		<div class="mono min-h-0 flex-1 overflow-auto bg-base-100 p-4 text-[13px] leading-6">
			{#each codeLines as line, index (index)}
				<div class="grid grid-cols-[3rem_minmax(0,1fr)] gap-3">
					<span class="select-none text-right text-base-content/50">{index + 1}</span>
					<code class="whitespace-pre text-base-content">{line}</code>
				</div>
			{/each}
		</div>
	</div>
</section>
