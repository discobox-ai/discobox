<script lang="ts">
	import PlusIcon from '@lucide/svelte/icons/plus';
	import { Badge } from '$lib/components/ui/badge/index.js';
	import { Button } from '$lib/components/ui/button/index.js';
	import { Input } from '$lib/components/ui/input/index.js';
	import type { Sandbox, SandboxGroup } from './types';

	let {
		activeSandbox,
		groups
	}: {
		activeSandbox: Sandbox;
		groups: SandboxGroup[];
	} = $props();

	function statusColorClass(status: Sandbox['status']) {
		if (status === 'running') return 'bg-emerald-500';
		if (status === 'review') return 'bg-primary';
		return 'bg-muted-foreground';
	}

	function statusLabel(status: Sandbox['status']) {
		if (status === 'review') return 'ready for review';
		return status;
	}

	function statusBadgeVariant(status: Sandbox['status']) {
		if (status === 'review') return 'default';
		return 'secondary';
	}
</script>

<aside class="flex min-h-full w-80 shrink-0 flex-col bg-background p-3 text-foreground">
	<div class="mb-3 flex items-center justify-between">
		<div>
			<p class="text-xs font-semibold uppercase tracking-[0.18em] text-muted-foreground">
				Sandboxes
			</p>
			<p class="text-xs text-muted-foreground">Grouped by directory</p>
		</div>
		<Button variant="ghost" size="sm" class="shrink-0">
			<PlusIcon class="size-3" />
			New Session
		</Button>
	</div>

	<Input class="mb-3 bg-background" placeholder="Search sandboxes, branches, providers…" />

	<div class="min-h-0 flex-1 overflow-auto pr-1">
		{#each groups as group (group.directory)}
			<section class="mb-4">
				<h2 class="mb-1 truncate px-2 text-sm font-medium text-muted-foreground">
					{group.directory}
				</h2>
				<div class="space-y-1">
					{#each group.items as sandbox (sandbox.id)}
						<button
							class={`block w-full rounded-lg px-3 py-2 text-left transition-colors ${sandbox.id === activeSandbox.id ? 'bg-tree-selected text-foreground' : 'hover:bg-tree-hover'}`}
						>
							<div class="flex items-center justify-between gap-2">
								<span class="truncate text-sm font-medium">{sandbox.name}</span>
								<span
									class={`h-1.5 w-1.5 shrink-0 rounded-full ${statusColorClass(sandbox.status)}`}
								></span>
							</div>
							<div
								class={`mt-1 flex items-center justify-between gap-2 text-xs ${sandbox.id === activeSandbox.id ? 'text-primary-foreground/70' : 'text-muted-foreground'}`}
							>
								<span class="truncate">⑂ {sandbox.branch}</span>
								<span>{sandbox.updated}</span>
							</div>
							<div class="mt-2 flex items-center gap-1.5">
								<Badge
									variant={sandbox.id === activeSandbox.id ? 'outline' : 'secondary'}
									class="h-4 px-1.5 text-[10px]"
								>
									{sandbox.provider}
								</Badge>
								<Badge variant={statusBadgeVariant(sandbox.status)} class="h-4 px-1.5 text-[10px]">
									{statusLabel(sandbox.status)}
								</Badge>
							</div>
						</button>
					{/each}
				</div>
			</section>
		{/each}
	</div>
</aside>
