<script lang="ts">
	import ChevronDownIcon from '@lucide/svelte/icons/chevron-down';
	import Code2Icon from '@lucide/svelte/icons/code-2';
	import FileCode2Icon from '@lucide/svelte/icons/file-code-2';
	import FilesIcon from '@lucide/svelte/icons/files';
	import GitBranchIcon from '@lucide/svelte/icons/git-branch';
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
	import type { ThemeColorScheme, ThemeMetadata, ThemeMode } from '$lib/theme';
	import ThemeSwitcher from './ThemeSwitcher.svelte';

	let {
		themeModes,
		themeMode,
		colorScheme,
		availableThemes,
		activeTheme,
		onThemeModeChange,
		onColorSchemeChange
	}: {
		themeModes: ThemeMode[];
		themeMode: ThemeMode;
		colorScheme: ThemeColorScheme;
		availableThemes: ThemeMetadata[];
		activeTheme: ThemeMetadata;
		onThemeModeChange: (mode: ThemeMode) => void;
		onColorSchemeChange: (scheme: ThemeColorScheme) => void;
	} = $props();
</script>

<header
	class="desktop-drag-region grid h-10 shrink-0 grid-cols-[auto_minmax(0,1fr)_auto] items-center border-b border-base-300 bg-base-100 text-base-content"
>
	<div class="relative z-10 flex min-w-0 items-center gap-2 pl-4 pr-3">
		<div
			class="desktop-no-drag mr-2 flex shrink-0 items-center gap-2"
			aria-label="macOS window controls"
		>
			<span class="size-3 rounded-full border border-black/10 bg-[#ff5f57] shadow-sm"></span>
			<span class="size-3 rounded-full border border-black/10 bg-[#febc2e] shadow-sm"></span>
			<span class="size-3 rounded-full border border-black/10 bg-[#28c840] shadow-sm"></span>
		</div>
		<img src={discobotBrand} alt="Discobot" class="h-6 w-auto shrink-0" />
	</div>

	<nav class="relative z-10 flex min-w-0 justify-center px-2" aria-label="Sandbox tools">
		<div
			class="desktop-no-drag flex min-w-0 items-center gap-2 overflow-hidden text-xs font-medium text-base-content/65"
		>
			<button
				class="inline-flex h-6 shrink-0 items-center gap-1 rounded-md px-2 text-base-content transition-colors"
			>
				<SquareTerminalIcon class="size-3.5" />
				Terminal
			</button>
			<button
				class="inline-flex h-6 shrink-0 items-center gap-1 rounded-md px-2 transition-colors hover:bg-base-content/10 hover:text-base-content"
			>
				<MonitorIcon class="size-3.5" />
				Desktop
			</button>
			<button
				class="inline-flex h-6 shrink-0 items-center gap-1 rounded-md px-2 transition-colors hover:bg-base-content/10 hover:text-base-content"
			>
				<Code2Icon class="size-3.5" />
				Editor
			</button>
			<button
				class="inline-flex h-6 shrink-0 items-center gap-1 rounded-md px-2 transition-colors hover:bg-base-content/10 hover:text-base-content"
			>
				<FilesIcon class="size-3.5" />
				Files
			</button>
			<button
				class="inline-flex h-6 shrink-0 items-center gap-1 rounded-md px-2 transition-colors hover:bg-base-content/10 hover:text-base-content"
			>
				<GitBranchIcon class="size-3.5" />
				<span class="text-success">+4536</span>
				<span class="text-error">-5</span>
				<span class="text-base-content/50">39</span>
			</button>
			<button
				class="inline-flex h-6 shrink-0 items-center gap-1 rounded-md bg-base-content/10 px-2 text-base-content transition-colors hover:bg-base-content/15"
			>
				<PlayIcon class="size-3.5 fill-current text-success" />
				Run
				<ChevronDownIcon class="size-3.5 text-base-content/50" />
			</button>
			<button
				class="inline-flex h-6 shrink-0 items-center gap-1 rounded-md px-2 transition-colors hover:bg-base-content/10 hover:text-base-content"
			>
				<SlidersHorizontalIcon class="size-3.5" />
				Commit
				<ChevronDownIcon class="size-3.5 text-base-content/50" />
			</button>
			<button
				class="inline-flex h-6 shrink-0 items-center gap-1 rounded-md px-2 transition-colors hover:bg-base-content/10 hover:text-base-content"
			>
				<FileCode2Icon class="size-3.5" />
				Open Zed
				<ChevronDownIcon class="size-3.5 text-base-content/50" />
			</button>
		</div>
	</nav>

	<div class="relative z-10 flex min-w-0 items-center justify-end gap-3 pr-3">
		<ThemeSwitcher
			{themeModes}
			{themeMode}
			{colorScheme}
			{availableThemes}
			{activeTheme}
			{onThemeModeChange}
			{onColorSchemeChange}
		/>
		<button
			class="desktop-no-drag inline-flex h-6 shrink-0 items-center gap-1 rounded-md px-2 text-xs font-medium text-base-content/65 transition-colors hover:bg-base-content/10 hover:text-base-content"
		>
			<PlusIcon class="size-3" />
			New Sandbox
		</button>
		<button
			class="desktop-no-drag rounded-md p-1 text-base-content/65 transition-colors hover:bg-base-content/10 hover:text-base-content"
			aria-label="Refresh"
		>
			<RefreshCwIcon class="size-4" />
		</button>
		<button
			class="desktop-no-drag rounded-md p-1 text-base-content/65 transition-colors hover:bg-base-content/10 hover:text-base-content"
			aria-label="Settings"
		>
			<SettingsIcon class="size-4" />
		</button>
		<div
			class="desktop-no-drag ml-1 flex h-10 shrink-0 items-stretch border-l border-base-300"
			aria-label="Windows window controls"
		>
			<button
				class="flex w-10 items-center justify-center text-base-content/65 transition-colors hover:bg-base-content/10 hover:text-base-content"
				aria-label="Minimize"
			>
				<MinusIcon class="size-4" />
			</button>
			<button
				class="flex w-10 items-center justify-center text-base-content/65 transition-colors hover:bg-base-content/10 hover:text-base-content"
				aria-label="Maximize"
			>
				<SquareIcon class="size-3" />
			</button>
			<button
				class="flex w-10 items-center justify-center text-base-content/65 transition-colors hover:bg-error hover:text-error-content"
				aria-label="Close"
			>
				<XIcon class="size-4" />
			</button>
		</div>
	</div>
</header>
