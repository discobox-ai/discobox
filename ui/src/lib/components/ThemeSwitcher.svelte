<script lang="ts">
	import type { ThemeColorScheme, ThemeMetadata, ThemeMode } from '$lib/theme';

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

<div class="desktop-no-drag flex items-center gap-1 text-xs font-medium">
	{#each themeModes as mode (mode)}
		<button
			type="button"
			class={`h-6 rounded-md px-2 capitalize transition-colors ${themeMode === mode ? 'bg-base-content/10 text-base-content' : 'text-base-content/50 hover:bg-base-content/10 hover:text-base-content/80'}`}
			onclick={() => onThemeModeChange(mode)}
		>
			{mode}
		</button>
	{/each}
</div>

<label
	class="desktop-no-drag flex h-6 w-32 items-center gap-1.5 rounded-md border border-base-300 bg-base-content/[0.03] px-1.5 text-xs text-base-content/70"
>
	<span
		class="h-3 w-3 shrink-0 rounded-full border border-base-content/20"
		style={`background: linear-gradient(135deg, ${activeTheme.preview.background} 0 45%, ${activeTheme.preview.primary} 45% 70%, ${activeTheme.preview.foreground} 70% 100%)`}
	></span>
	<span class="sr-only">Theme</span>
	<select
		class="min-w-0 grow bg-transparent text-xs font-medium text-base-content outline-none"
		value={colorScheme}
		onchange={(event) => onColorSchemeChange(event.currentTarget.value as ThemeColorScheme)}
	>
		{#each availableThemes as theme (`${theme.mode}:${theme.id}`)}
			<option value={theme.id}>{theme.name}</option>
		{/each}
	</select>
</label>
