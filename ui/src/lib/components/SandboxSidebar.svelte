<script lang="ts" module>
	import type { Sandbox } from './types';

	export type SandboxSidebarSort = 'updated' | 'name' | 'diff' | 'state';
	export type SandboxSidebarGrouping = 'none' | 'source' | 'state';
	export type SandboxSidebarTaskState = Sandbox['taskState'];
</script>

<script lang="ts">
	import CheckCircle2Icon from '@lucide/svelte/icons/check-circle-2';
	import CircleAlertIcon from '@lucide/svelte/icons/circle-alert';
	import CircleStopIcon from '@lucide/svelte/icons/circle-stop';
	import CircleDotIcon from '@lucide/svelte/icons/circle-dot';
	import EllipsisIcon from '@lucide/svelte/icons/ellipsis';
	import FolderIcon from '@lucide/svelte/icons/folder';
	import GitBranchIcon from '@lucide/svelte/icons/git-branch';
	import GitMergeIcon from '@lucide/svelte/icons/git-merge';
	import LoaderCircleIcon from '@lucide/svelte/icons/loader-circle';
	import PlayIcon from '@lucide/svelte/icons/play';
	import XCircleIcon from '@lucide/svelte/icons/x-circle';
	import { buttonVariants } from '$lib/components/ui/button/index.js';
	import * as DropdownMenu from '$lib/components/ui/dropdown-menu/index.js';
	import { cn } from '$lib/utils.js';
	import type { Component } from 'svelte';

	let {
		activeSandbox,
		sandboxes,
		homeDirectory,
		initialSort = 'updated',
		initialGrouping = 'source',
		initialVisibleStatuses = ['open', 'closed', 'merged']
	}: {
		activeSandbox: Sandbox;
		sandboxes: Sandbox[];
		homeDirectory?: string;
		initialSort?: SandboxSidebarSort;
		initialGrouping?: SandboxSidebarGrouping;
		initialVisibleStatuses?: SandboxSidebarTaskState[];
	} = $props();

	let sort = $state<SandboxSidebarSort>(getInitialSort());
	let grouping = $state<SandboxSidebarGrouping>(getInitialGrouping());
	let visibleStatuses = $state<Record<SandboxSidebarTaskState, boolean>>(
		getInitialVisibleStatuses()
	);

	const taskStates: SandboxSidebarTaskState[] = ['open', 'closed', 'merged'];
	const taskStateOrder: Record<SandboxSidebarTaskState, number> = {
		open: 0,
		merged: 1,
		closed: 2
	};

	const filteredSandboxes = $derived(
		sandboxes
			.filter((sandbox) => visibleStatuses[sandbox.taskState])
			.toSorted((a, b) => compareSandboxes(a, b, sort))
	);
	const sections = $derived(groupSandboxes(filteredSandboxes, grouping));
	const visibleCount = $derived(filteredSandboxes.length);

	function diffTotal(sandbox: Sandbox) {
		return sandbox.diff.additions + sandbox.diff.deletions;
	}

	function getInitialSort() {
		return initialSort;
	}

	function getInitialGrouping() {
		return initialGrouping;
	}

	function getInitialVisibleStatuses(): Record<SandboxSidebarTaskState, boolean> {
		return {
			open: initialVisibleStatuses.includes('open'),
			closed: initialVisibleStatuses.includes('closed'),
			merged: initialVisibleStatuses.includes('merged')
		};
	}

	function compareSandboxes(a: Sandbox, b: Sandbox, sortBy: SandboxSidebarSort) {
		if (sortBy === 'name') return a.name.localeCompare(b.name);
		if (sortBy === 'diff') return diffTotal(b) - diffTotal(a) || a.name.localeCompare(b.name);
		if (sortBy === 'state') {
			return (
				taskStateOrder[a.taskState] - taskStateOrder[b.taskState] || a.name.localeCompare(b.name)
			);
		}
		return parseUpdated(a.updated) - parseUpdated(b.updated) || a.name.localeCompare(b.name);
	}

	function parseUpdated(updated: string) {
		const match = updated.match(/^(\d+)([mhd]) ago$/);
		if (!match) return Number.MAX_SAFE_INTEGER;

		const value = Number(match[1]);
		const unit = match[2];
		if (unit === 'h') return value * 60;
		if (unit === 'd') return value * 60 * 24;
		return value;
	}

	function groupSandboxes(items: Sandbox[], groupBy: SandboxSidebarGrouping) {
		if (groupBy === 'none') return [{ label: 'All sandboxes', items }];
		if (groupBy === 'source') {
			const grouped: { label: string; items: Sandbox[] }[] = [];
			for (const sandbox of items) {
				const label = sourceInfo(sandbox).label;
				const section = grouped.find((entry) => entry.label === label);
				if (section) {
					section.items.push(sandbox);
				} else {
					grouped.push({ label, items: [sandbox] });
				}
			}

			return grouped.toSorted((a, b) => a.label.localeCompare(b.label));
		}

		return taskStates
			.map((state) => ({
				label: taskStateLabel(state),
				items: items.filter((sandbox) => sandbox.taskState === state)
			}))
			.filter((section) => section.items.length > 0);
	}

	function taskStateLabel(state: SandboxSidebarTaskState) {
		if (state === 'open') return 'Open';
		if (state === 'closed') return 'Closed';
		return 'Merged';
	}

	function sandboxStateLabel(sandbox: Sandbox) {
		if (sandbox.sandboxState === 'creating') return 'Creating';
		if (sandbox.sandboxState === 'running') return 'Running';
		if (sandbox.sandboxState === 'error') return 'Error';
		return 'Stopped';
	}

	function agentStatusLabel(sandbox: Sandbox) {
		if (sandbox.agentStatus === 'running_completion') return 'Running completion';
		if (sandbox.agentStatus === 'newly_idle') return 'Newly idle';
		return 'Idle';
	}

	function consolidatedStateLabel(sandbox: Sandbox) {
		if (sandbox.agentStatus === 'newly_idle') return 'Needs attention';
		if (sandbox.sandboxState === 'error') return 'Sandbox error';
		if (sandbox.sandboxState === 'creating') return 'Creating';
		if (sandbox.agentStatus === 'running_completion') return 'Agent running';
		if (sandbox.sandboxState === 'running') return 'Running';
		if (sandbox.taskState === 'merged') return 'Merged';
		if (sandbox.taskState === 'closed') return 'Closed';
		if (sandbox.sandboxState === 'stopped') return 'Stopped';
		return 'Open';
	}

	function consolidatedStateIcon(sandbox: Sandbox): Component {
		if (sandbox.agentStatus === 'newly_idle') return CircleAlertIcon;
		if (sandbox.sandboxState === 'error') return CircleAlertIcon;
		if (sandbox.sandboxState === 'creating') return LoaderCircleIcon;
		if (sandbox.agentStatus === 'running_completion') return PlayIcon;
		if (sandbox.sandboxState === 'running') return PlayIcon;
		if (sandbox.taskState === 'merged') return GitMergeIcon;
		if (sandbox.taskState === 'closed') return XCircleIcon;
		return sandbox.sandboxState === 'stopped' ? CircleStopIcon : CircleDotIcon;
	}

	function consolidatedStateTone(sandbox: Sandbox) {
		if (sandbox.agentStatus === 'newly_idle') return 'text-amber-600 dark:text-amber-300';
		if (sandbox.sandboxState === 'error') return 'text-destructive';
		if (sandbox.sandboxState === 'creating') return 'text-primary';
		if (sandbox.agentStatus === 'running_completion') return 'text-primary';
		if (sandbox.sandboxState === 'running') return 'text-emerald-600 dark:text-emerald-400';
		if (sandbox.taskState === 'merged') return 'text-emerald-600 dark:text-emerald-400';
		if (sandbox.taskState === 'closed') return 'text-muted-foreground';
		return 'text-muted-foreground';
	}

	function consolidatedStateTitle(sandbox: Sandbox) {
		const details = [
			consolidatedStateLabel(sandbox),
			`Task: ${taskStateLabel(sandbox.taskState)}`,
			`Sandbox: ${sandboxStateLabel(sandbox)}`,
			`Agent: ${agentStatusLabel(sandbox)}`
		];
		if (sandbox.agentStatusMessage) details.push(sandbox.agentStatusMessage);
		return details.join('\n');
	}

	function rowNeedsAttention(sandbox: Sandbox) {
		return sandbox.agentStatus === 'newly_idle' || sandbox.sandboxState === 'error';
	}

	function diffTone(value: number, type: 'additions' | 'deletions') {
		if (value === 0) return 'text-muted-foreground';
		return type === 'additions' ? 'text-[var(--diff-add-line)]' : 'text-[var(--diff-remove-line)]';
	}

	function sourceInfo(sandbox: Sandbox) {
		const source = sandbox.directory;

		if (source.startsWith('https://github.com/')) {
			return {
				kind: 'github',
				label: source.replace(/^https:\/\/github\.com\//, '').replace(/\.git$/, '')
			};
		}

		if (
			source.startsWith('http://') ||
			source.startsWith('https://') ||
			source.startsWith('git@') ||
			source.endsWith('.git')
		) {
			return {
				kind: 'git',
				label: source.replace(/\.git$/, '')
			};
		}

		return {
			kind: 'folder',
			label: shortenHomeDirectory(source)
		};
	}

	function shortenHomeDirectory(source: string) {
		if (!homeDirectory) return source;

		const normalizedHome = homeDirectory.replace(/\/+$/, '');
		if (!normalizedHome || source === normalizedHome) return '~';
		if (source.startsWith(`${normalizedHome}/`)) return `~${source.slice(normalizedHome.length)}`;
		return source;
	}
</script>

<aside
	class="flex h-full min-h-0 min-w-80 flex-1 flex-col overflow-hidden bg-background text-foreground"
>
	<div class="shrink-0 px-3 pt-3 pb-1">
		<div class="flex items-start justify-between gap-3">
			<div class="min-w-0">
				<p class="text-xs font-semibold uppercase tracking-[0.18em] text-muted-foreground">
					Sandboxes
				</p>
			</div>

			<DropdownMenu.Root>
				<DropdownMenu.Trigger
					aria-label="Sandbox sidebar options"
					class={cn(buttonVariants({ variant: 'ghost', size: 'icon-sm' }), 'shrink-0')}
				>
					<EllipsisIcon class="size-4" />
				</DropdownMenu.Trigger>
				<DropdownMenu.Content align="end" class="w-56">
					<DropdownMenu.Label>Sort order</DropdownMenu.Label>
					<DropdownMenu.RadioGroup bind:value={sort}>
						<DropdownMenu.RadioItem value="updated">Recently updated</DropdownMenu.RadioItem>
						<DropdownMenu.RadioItem value="name">Name</DropdownMenu.RadioItem>
						<DropdownMenu.RadioItem value="diff">Largest diff</DropdownMenu.RadioItem>
						<DropdownMenu.RadioItem value="state">State</DropdownMenu.RadioItem>
					</DropdownMenu.RadioGroup>
					<DropdownMenu.Separator />
					<DropdownMenu.Label>Grouping</DropdownMenu.Label>
					<DropdownMenu.RadioGroup bind:value={grouping}>
						<DropdownMenu.RadioItem value="source">Source</DropdownMenu.RadioItem>
						<DropdownMenu.RadioItem value="state">State</DropdownMenu.RadioItem>
						<DropdownMenu.RadioItem value="none">None</DropdownMenu.RadioItem>
					</DropdownMenu.RadioGroup>
					<DropdownMenu.Separator />
					<DropdownMenu.Label>Task states</DropdownMenu.Label>
					{#each taskStates as state (state)}
						<DropdownMenu.CheckboxItem bind:checked={visibleStatuses[state]}>
							{taskStateLabel(state)}
						</DropdownMenu.CheckboxItem>
					{/each}
				</DropdownMenu.Content>
			</DropdownMenu.Root>
		</div>
	</div>

	<div class="min-h-0 flex-1 overflow-auto px-2 pt-1 pb-3">
		{#if visibleCount === 0}
			<div class="rounded-md border border-dashed border-border px-3 py-8 text-center">
				<CheckCircle2Icon class="mx-auto mb-2 size-5 text-muted-foreground" />
				<p class="text-sm font-medium">No sandboxes</p>
				<p class="mt-1 text-xs text-muted-foreground">Adjust the state filter to show results.</p>
			</div>
		{:else}
			{#each sections as section (section.label)}
				<section class="mb-3 last:mb-0">
					{#if grouping !== 'none'}
						<h2
							class="mb-1 px-2 text-xs font-semibold uppercase tracking-[0.14em] text-muted-foreground"
						>
							{section.label}
						</h2>
					{/if}

					<div class="space-y-1">
						{#each section.items as sandbox (sandbox.id)}
							{@const ConsolidatedStateIcon = consolidatedStateIcon(sandbox)}
							{@const source = sourceInfo(sandbox)}
							<div
								class={cn(
									'w-full rounded-md border px-2.5 py-2 text-left transition-colors',
									sandbox.id === activeSandbox.id
										? rowNeedsAttention(sandbox)
											? 'border-amber-500/45 bg-amber-500/10'
											: 'border-primary/35 bg-tree-selected'
										: 'border-transparent hover:border-border hover:bg-tree-hover'
								)}
							>
								<div class="flex min-w-0 items-start justify-between gap-2">
									<div class="min-w-0">
										<div class="flex min-w-0 items-center gap-1.5">
											<span
												class="inline-flex size-4 shrink-0 items-center justify-center"
												title={consolidatedStateTitle(sandbox)}
												aria-label={consolidatedStateTitle(sandbox)}
											>
												<ConsolidatedStateIcon
													class={cn('size-3.5', consolidatedStateTone(sandbox))}
												/>
											</span>
											<span class="truncate text-sm font-medium">{sandbox.name}</span>
										</div>
									</div>

									<DropdownMenu.Root>
										<DropdownMenu.Trigger
											aria-label={`Actions for ${sandbox.name}`}
											class={cn(
												buttonVariants({ variant: 'ghost', size: 'icon-xs' }),
												'-mt-1 -mr-1 shrink-0 text-muted-foreground hover:text-foreground'
											)}
										>
											<EllipsisIcon class="size-3.5" />
										</DropdownMenu.Trigger>
										<DropdownMenu.Content align="end" class="w-36">
											<DropdownMenu.Item>Shutdown</DropdownMenu.Item>
											<DropdownMenu.Item class="text-destructive focus:text-destructive">
												Delete
											</DropdownMenu.Item>
										</DropdownMenu.Content>
									</DropdownMenu.Root>
								</div>

								<div class="mt-2 flex min-w-0 items-center gap-2 text-[11px] text-muted-foreground">
									<div
										class="flex min-w-0 flex-1 items-center gap-1.5"
										title={`${source.label}@${sandbox.branch}`}
									>
										{#if source.kind === 'github'}
											<svg
												viewBox="0 0 24 24"
												aria-hidden="true"
												class="size-3.5 shrink-0 fill-current text-muted-foreground"
											>
												<path
													d="M12 .5a12 12 0 0 0-3.79 23.39c.6.11.82-.26.82-.58v-2.04c-3.34.73-4.04-1.61-4.04-1.61-.55-1.39-1.34-1.76-1.34-1.76-1.09-.75.08-.73.08-.73 1.2.08 1.84 1.24 1.84 1.24 1.07 1.83 2.81 1.3 3.49.99.11-.78.42-1.3.76-1.6-2.67-.3-5.47-1.33-5.47-5.93 0-1.31.47-2.38 1.24-3.22-.12-.3-.54-1.52.12-3.18 0 0 1.01-.32 3.3 1.23a11.5 11.5 0 0 1 6 0c2.29-1.55 3.3-1.23 3.3-1.23.66 1.66.24 2.88.12 3.18.77.84 1.24 1.91 1.24 3.22 0 4.61-2.81 5.62-5.48 5.92.43.37.81 1.1.81 2.22v3.29c0 .32.22.7.83.58A12 12 0 0 0 12 .5Z"
												/>
											</svg>
										{:else if source.kind === 'git'}
											<GitBranchIcon class="size-3.5 shrink-0 text-muted-foreground" />
										{:else}
											<FolderIcon class="size-3.5 shrink-0 text-muted-foreground" />
										{/if}
										<span class="min-w-0 truncate">
											<span>{source.label}</span><span class="text-muted-foreground/60">@</span
											><span class="font-mono"> {sandbox.branch}</span>
										</span>
									</div>

									<div class="flex shrink-0 items-center gap-1.5">
										<span>{sandbox.updated}</span>
										<span class="text-muted-foreground/60">·</span>
										<span class="shrink-0">{sandbox.diff.files} files</span>
										<span class="text-muted-foreground/60">·</span>
										<span class="shrink-0 font-mono">
											<span class={diffTone(sandbox.diff.additions, 'additions')}>
												+{sandbox.diff.additions}
											</span>
											<span class="mx-1 text-muted-foreground/60">/</span>
											<span class={diffTone(sandbox.diff.deletions, 'deletions')}>
												-{sandbox.diff.deletions}
											</span>
										</span>
									</div>
								</div>
							</div>
						{/each}
					</div>
				</section>
			{/each}
		{/if}
	</div>
</aside>
