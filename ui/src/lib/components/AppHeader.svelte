<script lang="ts">
	import ChevronDownIcon from '@lucide/svelte/icons/chevron-down';
	import Code2Icon from '@lucide/svelte/icons/code-2';
	import FileCode2Icon from '@lucide/svelte/icons/file-code-2';
	import FilesIcon from '@lucide/svelte/icons/files';
	import GitBranchIcon from '@lucide/svelte/icons/git-branch';
	import PanelLeftIcon from '@lucide/svelte/icons/panel-left';
	import PlayIcon from '@lucide/svelte/icons/play';
	import PlusIcon from '@lucide/svelte/icons/plus';
	import RefreshCwIcon from '@lucide/svelte/icons/refresh-cw';
	import SettingsIcon from '@lucide/svelte/icons/settings';
	import SlidersHorizontalIcon from '@lucide/svelte/icons/sliders-horizontal';
	import SquareTerminalIcon from '@lucide/svelte/icons/square-terminal';
	import MonitorIcon from '@lucide/svelte/icons/monitor';
	import discobotBrand from '$lib/assets/branding/discobot-brand-gradient.svg';
	import { Button } from '$lib/components/ui/button/index.js';
	import type { WindowControlsMode } from '$lib/environment';

	let {
		sidebarCollapsed,
		windowControls = 'macos',
		onSidebarToggle,
		onSettingsOpen,
		onTitleBarDoubleClick
	}: {
		sidebarCollapsed: boolean;
		windowControls?: WindowControlsMode;
		onSidebarToggle: () => void;
		onSettingsOpen: () => void;
		onTitleBarDoubleClick?: () => void;
	} = $props();

	const reserveLeftNativeControls = $derived(windowControls === 'macos');
	const reserveRightNativeControls = $derived(
		windowControls === 'windows' || windowControls === 'linux'
	);

	function handleTitleBarDoubleClick(event: MouseEvent) {
		const target = event.target;
		if (target instanceof Element && target.closest('.desktop-no-drag')) {
			return;
		}

		onTitleBarDoubleClick?.();
	}
</script>

<div
	class="desktop-drag-region relative z-[60] grid h-10 w-full shrink-0 grid-cols-[auto_minmax(0,1fr)_auto] items-center bg-background text-foreground"
	role="banner"
	aria-label="Application title bar"
	ondblclick={handleTitleBarDoubleClick}
>
	<div
		class={`relative z-10 flex min-w-0 items-center gap-2 ${reserveLeftNativeControls ? 'pl-0' : 'pl-3'} pr-3`}
	>
		{#if reserveLeftNativeControls}
			<div
				class="desktop-no-drag h-10 w-[76px] shrink-0"
				aria-hidden="true"
				title="Native macOS window controls"
			></div>
		{/if}
		<Button
			type="button"
			variant="ghost"
			size="icon-xs"
			class="desktop-no-drag"
			aria-label={sidebarCollapsed ? 'Show sandboxes sidebar' : 'Hide sandboxes sidebar'}
			aria-pressed={!sidebarCollapsed}
			onclick={onSidebarToggle}
		>
			<PanelLeftIcon class="size-3.5" />
		</Button>
		<img src={discobotBrand} alt="Discobot" class="h-6 w-auto shrink-0 -translate-y-0.5" />
	</div>

	<nav class="relative z-10 flex min-w-0 justify-center px-2" aria-label="Sandbox tools">
		<div
			class="desktop-no-drag flex min-w-0 items-center gap-1 overflow-hidden text-xs text-muted-foreground"
		>
			<button
				class="inline-flex h-6 shrink-0 items-center gap-1 rounded-md bg-muted px-2 text-foreground transition-colors"
			>
				<SquareTerminalIcon class="size-3.5" />
				Terminal
			</button>
			<button
				class="inline-flex h-6 shrink-0 items-center gap-1 rounded-md px-2 transition-colors hover:bg-muted hover:text-foreground"
			>
				<MonitorIcon class="size-3.5" />
				Desktop
			</button>
			<button
				class="inline-flex h-6 shrink-0 items-center gap-1 rounded-md px-2 transition-colors hover:bg-muted hover:text-foreground"
			>
				<Code2Icon class="size-3.5" />
				Editor
			</button>
			<button
				class="inline-flex h-6 shrink-0 items-center gap-1 rounded-md px-2 transition-colors hover:bg-muted hover:text-foreground"
			>
				<FilesIcon class="size-3.5" />
				Files
			</button>
			<button
				class="inline-flex h-6 shrink-0 items-center gap-1 rounded-md px-2 transition-colors hover:bg-muted hover:text-foreground"
			>
				<GitBranchIcon class="size-3.5" />
				<span class="text-emerald-500">+4536</span>
				<span class="text-destructive">-5</span>
				<span class="text-muted-foreground">39</span>
			</button>
			<button
				class="inline-flex h-6 shrink-0 items-center gap-1 rounded-md px-2 transition-colors hover:bg-muted hover:text-foreground"
			>
				<PlayIcon class="size-3.5 fill-current text-emerald-500" />
				Run
				<ChevronDownIcon class="size-3.5 text-muted-foreground" />
			</button>
			<button
				class="inline-flex h-6 shrink-0 items-center gap-1 rounded-md px-2 transition-colors hover:bg-muted hover:text-foreground"
			>
				<SlidersHorizontalIcon class="size-3.5" />
				Commit
				<ChevronDownIcon class="size-3.5 text-muted-foreground" />
			</button>
			<button
				class="inline-flex h-6 shrink-0 items-center gap-1 rounded-md px-2 transition-colors hover:bg-muted hover:text-foreground"
			>
				<FileCode2Icon class="size-3.5" />
				Open Zed
				<ChevronDownIcon class="size-3.5 text-muted-foreground" />
			</button>
		</div>
	</nav>

	<div
		class={`relative z-10 flex min-w-0 items-center justify-end gap-2 ${reserveRightNativeControls ? 'pr-0' : 'pr-3'}`}
	>
		<div class="relative hidden size-6 shrink-0 md:block">
			<Button
				variant="ghost"
				size="xs"
				class="desktop-no-drag group/new-sandbox absolute right-0 top-0 w-6 overflow-hidden px-0 transition-all duration-200 hover:w-28 hover:px-2 focus-visible:w-28 focus-visible:px-2"
				aria-label="New Sandbox"
			>
				<PlusIcon class="size-3" />
				<span
					class="max-w-0 overflow-hidden opacity-0 transition-all duration-200 group-hover/new-sandbox:max-w-24 group-hover/new-sandbox:opacity-100 group-focus-visible/new-sandbox:max-w-24 group-focus-visible/new-sandbox:opacity-100"
				>
					New Sandbox
				</span>
			</Button>
		</div>
		<Button
			variant="ghost"
			size="icon-xs"
			class="desktop-no-drag"
			aria-label="Window Reload"
			title="Window Reload"
		>
			<RefreshCwIcon class="size-3.5" />
		</Button>
		<Button
			type="button"
			variant="ghost"
			size="icon-xs"
			class="desktop-no-drag"
			aria-label="Settings"
			onclick={onSettingsOpen}
		>
			<SettingsIcon class="size-3.5" />
		</Button>
		{#if reserveRightNativeControls}
			<div
				class="desktop-no-drag hidden h-10 shrink-0 sm:block"
				style="width: env(titlebar-area-width, 138px)"
				aria-hidden="true"
				title="Native window controls"
			></div>
		{/if}
	</div>
</div>
