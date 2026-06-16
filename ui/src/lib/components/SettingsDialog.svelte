<script lang="ts">
	import { Button } from '$lib/components/ui/button/index.js';
	import * as Dialog from '$lib/components/ui/dialog/index.js';
	import { NativeSelect } from '$lib/components/ui/native-select/index.js';
	import type { ThemeColorScheme, ThemeMetadata, ThemeMode } from '$lib/theme';

	let {
		modalId,
		open,
		themeModes,
		themeMode,
		colorScheme,
		availableThemes,
		activeTheme,
		onThemeModeChange,
		onColorSchemeChange,
		onClose
	}: {
		modalId: string;
		open: boolean;
		themeModes: ThemeMode[];
		themeMode: ThemeMode;
		colorScheme: ThemeColorScheme;
		availableThemes: ThemeMetadata[];
		activeTheme: ThemeMetadata;
		onThemeModeChange: (mode: ThemeMode) => void;
		onColorSchemeChange: (scheme: ThemeColorScheme) => void;
		onClose: () => void;
	} = $props();
</script>

<Dialog.Root {open} onOpenChange={(nextOpen) => !nextOpen && onClose()}>
	<Dialog.Content
		id={modalId}
		class="max-w-lg sm:max-w-lg"
		aria-describedby={`${modalId}-description`}
	>
		<Dialog.Header>
			<Dialog.Title id={`${modalId}-title`}>Settings</Dialog.Title>
			<Dialog.Description id={`${modalId}-description`}>
				Tune the local prototype without connecting to backend services.
			</Dialog.Description>
		</Dialog.Header>

		<section class="rounded-xl border border-border bg-muted/40 p-4">
			<div class="mb-4">
				<h3 class="text-sm font-semibold">Appearance</h3>
				<p class="mt-1 text-xs text-muted-foreground">Theme preferences are stored locally.</p>
			</div>
			<div class="space-y-4">
				<div>
					<div class="mb-2 text-xs font-medium text-muted-foreground">Mode</div>
					<div class="grid grid-cols-3 rounded-lg border border-border bg-background p-1">
						{#each themeModes as mode (mode)}
							<Button
								variant={themeMode === mode ? 'secondary' : 'ghost'}
								size="sm"
								class="capitalize"
								onclick={() => onThemeModeChange(mode)}
							>
								{mode}
							</Button>
						{/each}
					</div>
				</div>

				<label class="block">
					<span class="mb-2 block text-xs font-medium text-muted-foreground">Theme</span>
					<div class="flex items-center gap-2">
						<span
							class="h-5 w-5 shrink-0 rounded-full border border-border"
							style={`background: linear-gradient(135deg, ${activeTheme.preview.background} 0 45%, ${activeTheme.preview.primary} 45% 70%, ${activeTheme.preview.foreground} 70% 100%)`}
						></span>
						<NativeSelect
							class="min-w-0 grow"
							size="sm"
							value={colorScheme}
							onchange={(event) =>
								onColorSchemeChange(event.currentTarget.value as ThemeColorScheme)}
						>
							{#each availableThemes as theme (`${theme.mode}:${theme.id}`)}
								<option value={theme.id}>{theme.name}</option>
							{/each}
						</NativeSelect>
					</div>
				</label>
			</div>
		</section>

		<Dialog.Footer>
			<Button type="button" size="sm" onclick={onClose}>Done</Button>
		</Dialog.Footer>
	</Dialog.Content>
</Dialog.Root>
