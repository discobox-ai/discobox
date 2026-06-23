<script lang="ts">
	import MinusIcon from '@lucide/svelte/icons/minus';
	import SquareIcon from '@lucide/svelte/icons/square';
	import XIcon from '@lucide/svelte/icons/x';
	import type { WindowControlsStyle } from './window-controls';

	let {
		style,
		maximized = false
	}: {
		style: WindowControlsStyle;
		maximized?: boolean;
	} = $props();

	const label = $derived(
		style === 'macos'
			? 'macOS window controls'
			: style === 'linux'
				? 'Linux window controls'
				: 'Windows window controls'
	);
</script>

{#if style === 'macos'}
	<div class="desktop-no-drag mr-1 flex shrink-0 items-center gap-2" aria-label={label}>
		<span class="size-3 rounded-full border border-black/10 bg-[#ff5f57] shadow-sm"></span>
		<span class="size-3 rounded-full border border-black/10 bg-[#febc2e] shadow-sm"></span>
		<span class="size-3 rounded-full border border-black/10 bg-[#28c840] shadow-sm"></span>
	</div>
{:else}
	<div class="desktop-no-drag hidden h-10 shrink-0 items-stretch sm:flex" aria-label={label}>
		<button
			class="flex w-10 items-center justify-center text-muted-foreground transition-colors hover:bg-muted hover:text-foreground"
			aria-label="Minimize"
		>
			<MinusIcon class="size-4" />
		</button>
		<button
			class="flex w-10 items-center justify-center text-muted-foreground transition-colors hover:bg-muted hover:text-foreground"
			aria-label={maximized ? 'Restore' : 'Maximize'}
		>
			{#if maximized}
				<svg
					class="size-3.5"
					viewBox="0 0 16 16"
					fill="none"
					stroke="currentColor"
					stroke-width="1.5"
					aria-hidden="true"
				>
					<path d="M5.5 3.5h7v7h-2" />
					<path d="M3.5 5.5h7v7h-7z" />
				</svg>
			{:else}
				<SquareIcon class="size-3" />
			{/if}
		</button>
		<button
			class="flex w-10 items-center justify-center text-muted-foreground transition-colors hover:bg-destructive hover:text-white"
			aria-label="Close"
		>
			<XIcon class="size-4" />
		</button>
	</div>
{/if}
