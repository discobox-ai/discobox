<script lang="ts">
	import { onMount } from 'svelte';
	import AppHeader from '$lib/components/AppHeader.svelte';
	import SandboxSidebar from '$lib/components/SandboxSidebar.svelte';
	import SandboxWorkspace from '$lib/components/SandboxWorkspace.svelte';
	import type { Sandbox, WorkspaceFile } from '$lib/components/types';
	import {
		applyColorScheme,
		applyThemePreferences,
		getAvailableThemes,
		getColorScheme,
		getThemeMetadata,
		getThemeMode,
		type ThemeColorScheme,
		type ThemeMode
	} from '$lib/theme';

	const sandboxes: Sandbox[] = [
		{
			id: 'sbx-api-042',
			directory: '/home/discobot/workspace',
			name: 'api-reconcile-flow',
			branch: 'feature/reconcile-events',
			status: 'running',
			updated: '2m ago',
			provider: 'Docker'
		},
		{
			id: 'sbx-ui-117',
			directory: '/home/discobot/workspace/ui',
			name: 'sandbox-shell-mock',
			branch: 'feature/ui-shell',
			status: 'review',
			updated: '8m ago',
			provider: 'Docker'
		},
		{
			id: 'sbx-agent-009',
			directory: '/home/discobot/experiments/agent',
			name: 'worker-agent-test',
			branch: 'main',
			status: 'paused',
			updated: '21m ago',
			provider: 'Firecracker'
		},
		{
			id: 'sbx-docs-031',
			directory: '/home/discobot/experiments/docs',
			name: 'docs-openapi-pass',
			branch: 'docs/api-examples',
			status: 'running',
			updated: '34m ago',
			provider: 'Docker'
		}
	];

	const groupedSandboxes = Object.entries(
		sandboxes.reduce<Record<string, Sandbox[]>>((groups, sandbox) => {
			groups[sandbox.directory] ??= [];
			groups[sandbox.directory].push(sandbox);
			return groups;
		}, {})
	)
		.sort(([directoryA], [directoryB]) => directoryA.localeCompare(directoryB))
		.map(([directory, items]) => ({
			directory,
			items: items.toSorted((a, b) => a.name.localeCompare(b.name))
		}));

	const activeSandbox = sandboxes[1];
	const themeModes: ThemeMode[] = ['light', 'dark', 'system'];
	let themeMode = $state<ThemeMode>('system');
	let colorScheme = $state<ThemeColorScheme>('light');
	let resolvedTheme = $state<'dark' | 'light'>('dark');
	const availableThemes = $derived(getAvailableThemes(resolvedTheme));
	const activeTheme = $derived(getThemeMetadata(resolvedTheme, colorScheme));

	const files: WorkspaceFile[] = [
		{ name: 'internal/service/sandbox.go', state: 'M' },
		{ name: 'internal/store/sandbox.go', state: 'M' },
		{ name: 'ui/src/routes/+page.svelte', state: 'A' },
		{ name: 'Taskfile.yml', state: 'M' }
	];

	const codeLines = [
		'type Sandbox struct {',
		'    ID          string    `json:"id"`',
		'    Directory   string    `json:"directory"`',
		'    Provider    string    `json:"provider"`',
		'    Generation  int64     `json:"generation"`',
		'    CreatedAt   time.Time `json:"createdAt"`',
		'}',
		'',
		'func (s Sandbox) DisplayPath() string {',
		'    return filepath.Clean(s.Directory)',
		'}'
	];

	const terminalLines = [
		'$ go tool task dev:server',
		'[discobox] watching Docker worker image inputs',
		'[air] building ./cmd/discobox-server',
		'[server] listening on :8080',
		'$ pnpm --dir ui run dev --host 0.0.0.0 --port 5173 --strictPort',
		'VITE v8.0.16  ready in 412 ms',
		'➜  Local:   http://localhost:5173/',
		'➜  Network: http://172.18.0.2:5173/'
	];

	function setAppliedThemePreferences(mode: ThemeMode, scheme: ThemeColorScheme) {
		const preferences = applyThemePreferences(mode, scheme);
		themeMode = preferences.theme;
		resolvedTheme = preferences.resolvedTheme;
		colorScheme = preferences.colorScheme;
	}

	function setThemeMode(mode: ThemeMode) {
		setAppliedThemePreferences(mode, colorScheme);
	}

	function setColorScheme(scheme: ThemeColorScheme) {
		colorScheme = applyColorScheme(scheme);
	}

	onMount(() => {
		setAppliedThemePreferences(getThemeMode(), getColorScheme());

		const media = window.matchMedia('(prefers-color-scheme: dark)');
		const handleSystemThemeChange = () => {
			if (themeMode === 'system') {
				setAppliedThemePreferences(themeMode, colorScheme);
			}
		};

		media.addEventListener('change', handleSystemThemeChange);
		return () => media.removeEventListener('change', handleSystemThemeChange);
	});
</script>

<svelte:head>
	<title>Discobot Sandbox Mock</title>
</svelte:head>

<div class="flex h-[100dvh] flex-col overflow-hidden bg-base-100 text-base-content">
	<AppHeader
		{themeModes}
		{themeMode}
		{colorScheme}
		{availableThemes}
		{activeTheme}
		onThemeModeChange={setThemeMode}
		onColorSchemeChange={setColorScheme}
	/>

	<div class="flex min-h-0 flex-1 overflow-hidden">
		<SandboxSidebar {activeSandbox} groups={groupedSandboxes} />
		<SandboxWorkspace {activeSandbox} {files} {codeLines} {terminalLines} />
	</div>
</div>
