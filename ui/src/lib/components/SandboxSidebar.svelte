<script lang="ts">
	import PlusIcon from '@lucide/svelte/icons/plus';
	import type { Sandbox, SandboxGroup } from './types';

	let {
		activeSandbox,
		groups
	}: {
		activeSandbox: Sandbox;
		groups: SandboxGroup[];
	} = $props();

	function statusClass(status: Sandbox['status']) {
		if (status === 'running') return 'status-success';
		if (status === 'review') return 'status-primary';
		return 'status-neutral';
	}

	function statusLabel(status: Sandbox['status']) {
		if (status === 'review') return 'ready for review';
		return status;
	}
</script>

<aside
	class="min-h-0 w-[320px] shrink-0 border-r border-base-300 bg-base-200 p-3 text-base-content"
>
	<div class="mb-3 flex items-center justify-between">
		<div>
			<p class="text-xs font-semibold uppercase tracking-[0.18em] text-base-content/60">
				Sandboxes
			</p>
			<p class="text-xs text-base-content/60">Grouped by directory</p>
		</div>
		<button
			class="inline-flex h-6 shrink-0 items-center gap-1 rounded-md px-2 text-xs font-medium text-base-content/65 transition-colors hover:bg-base-content/10 hover:text-base-content"
		>
			<PlusIcon class="size-3" />
			New Session
		</button>
	</div>

	<label class="input input-bordered input-sm mb-3 flex w-full items-center bg-base-100 shadow-sm">
		<input class="grow" placeholder="Search sandboxes, branches, providers…" />
	</label>

	<div class="flex h-[calc(100%-6.5rem)] flex-col gap-3 overflow-auto pr-1">
		{#each groups as group (group.directory)}
			<section>
				<div
					class="mb-1 flex items-center gap-1.5 px-1 text-[11px] font-semibold uppercase tracking-[0.14em] text-base-content/60"
				>
					<span>▾</span>
					<span class="truncate">{group.directory}</span>
				</div>
				<div class="space-y-1">
					{#each group.items as sandbox (sandbox.id)}
						<button
							class={`w-full rounded-lg border px-3 py-2 text-left transition ${sandbox.id === activeSandbox.id ? 'border-primary bg-primary/15 shadow-sm' : 'border-transparent hover:bg-base-300'}`}
						>
							<div class="flex items-center justify-between gap-2">
								<span class="truncate text-sm font-medium">{sandbox.name}</span>
								<span class={`status status-xs shrink-0 ${statusClass(sandbox.status)}`}></span>
							</div>
							<div
								class="mt-1 flex items-center justify-between gap-2 text-xs text-base-content/60"
							>
								<span class="truncate">⑂ {sandbox.branch}</span>
								<span>{sandbox.updated}</span>
							</div>
							<div class="mt-1 text-[11px] uppercase tracking-[0.12em] text-base-content/60">
								{sandbox.provider} · {statusLabel(sandbox.status)}
							</div>
						</button>
					{/each}
				</div>
			</section>
		{/each}
	</div>
</aside>
