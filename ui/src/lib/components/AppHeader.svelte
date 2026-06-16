<script lang="ts">
	import ChevronDownIcon from '@lucide/svelte/icons/chevron-down';
	import Code2Icon from '@lucide/svelte/icons/code-2';
	import FileCode2Icon from '@lucide/svelte/icons/file-code-2';
	import FilesIcon from '@lucide/svelte/icons/files';
	import GitBranchIcon from '@lucide/svelte/icons/git-branch';
	import MenuIcon from '@lucide/svelte/icons/menu';
	import MinusIcon from '@lucide/svelte/icons/minus';
	import PlayIcon from '@lucide/svelte/icons/play';
	import PlusIcon from '@lucide/svelte/icons/plus';
	import RefreshCwIcon from '@lucide/svelte/icons/refresh-cw';
	import SettingsIcon from '@lucide/svelte/icons/settings';
	import SlidersHorizontalIcon from '@lucide/svelte/icons/sliders-horizontal';
	import SquareIcon from '@lucide/svelte/icons/square';
	import SquareTerminalIcon from '@lucide/svelte/icons/square-terminal';
	import MonitorIcon from '@lucide/svelte/icons/monitor';
	import XIcon from '@lucide/svelte/icons/x';
	import discobotBrand from '$lib/assets/branding/discobot-brand-gradient.svg';
	import { Button } from '$lib/components/ui/button/index.js';

	let {
		sidebarCollapsed,
		onSidebarToggle,
		onSettingsOpen
	}: {
		sidebarCollapsed: boolean;
		onSidebarToggle: () => void;
		onSettingsOpen: () => void;
	} = $props();
</script>

<header
	class="desktop-drag-region relative grid h-10 w-full shrink-0 grid-cols-[auto_minmax(0,1fr)_auto] items-center bg-background text-foreground"
>
	<div class="relative z-10 flex min-w-0 items-center gap-2 pl-4 pr-3">
		<div
			class="desktop-no-drag mr-1 flex shrink-0 items-center gap-2"
			aria-label="macOS window controls"
		>
			<span class="size-3 rounded-full border border-black/10 bg-[#ff5f57] shadow-sm"></span>
			<span class="size-3 rounded-full border border-black/10 bg-[#febc2e] shadow-sm"></span>
			<span class="size-3 rounded-full border border-black/10 bg-[#28c840] shadow-sm"></span>
		</div>
		<Button
			type="button"
			variant="ghost"
			size="icon-xs"
			class="desktop-no-drag"
			aria-label={sidebarCollapsed ? 'Show sandboxes sidebar' : 'Hide sandboxes sidebar'}
			aria-pressed={!sidebarCollapsed}
			onclick={onSidebarToggle}
		>
			<MenuIcon class="size-3.5" />
		</Button>
		<img src={discobotBrand} alt="Discobot" class="h-6 w-auto shrink-0" />
	</div>

	<nav class="relative z-10 flex min-w-0 justify-center px-2" aria-label="Sandbox tools">
		<div
			class="desktop-no-drag flex min-w-0 items-center gap-1 overflow-hidden text-xs font-medium text-muted-foreground"
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

	<div class="relative z-10 flex min-w-0 items-center justify-end gap-2 pr-3">
		<Button variant="ghost" size="xs" class="desktop-no-drag hidden shrink-0 md:inline-flex">
			<PlusIcon class="size-3" />
			New Sandbox
		</Button>
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
		<div
			class="desktop-no-drag ml-1 hidden h-10 shrink-0 items-stretch border-l border-border sm:flex"
			aria-label="Windows window controls"
		>
			<button
				class="flex w-10 items-center justify-center text-muted-foreground transition-colors hover:bg-muted hover:text-foreground"
				aria-label="Minimize"
			>
				<MinusIcon class="size-4" />
			</button>
			<button
				class="flex w-10 items-center justify-center text-muted-foreground transition-colors hover:bg-muted hover:text-foreground"
				aria-label="Maximize"
			>
				<SquareIcon class="size-3" />
			</button>
			<button
				class="flex w-10 items-center justify-center text-muted-foreground transition-colors hover:bg-destructive hover:text-white"
				aria-label="Close"
			>
				<XIcon class="size-4" />
			</button>
		</div>
	</div>
</header>
