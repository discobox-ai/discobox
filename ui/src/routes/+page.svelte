<script lang="ts">
	import { onMount } from 'svelte';
	import AppHeader from '$lib/components/AppHeader.svelte';
	import SandboxSidebar from '$lib/components/SandboxSidebar.svelte';
	import SandboxWorkspace from '$lib/components/SandboxWorkspace.svelte';
	import SettingsDialog from '$lib/components/SettingsDialog.svelte';
	import type { Sandbox, WorkspaceFile } from '$lib/components/types';
	import { createContext, setContext } from '$lib/context';
	import { getAppEnvironment } from '$lib/environment';
	import { getThemeMetadata, type ThemeColorScheme, type ThemeMode } from '$lib/theme';

	const sandboxes: Sandbox[] = [
		{
			id: 'sbx-api-042',
			directory: '/home/discobot/workspace',
			name: 'api-reconcile-flow',
			branch: 'feature/reconcile-events',
			taskState: 'open',
			sandboxState: 'running',
			agentStatus: 'newly_idle',
			agentStatusMessage: 'Session is waiting for an answer',
			updated: '2m ago',
			provider: 'Docker',
			diff: { files: 14, additions: 428, deletions: 96 }
		},
		{
			id: 'sbx-ui-117',
			directory: '/home/discobot/workspace/ui',
			name: 'sandbox-shell-mock',
			branch: 'feature/ui-shell',
			taskState: 'open',
			sandboxState: 'running',
			agentStatus: 'running_completion',
			updated: '8m ago',
			provider: 'Docker',
			diff: { files: 8, additions: 281, deletions: 74 }
		},
		{
			id: 'sbx-agent-009',
			directory: '/home/discobot/experiments/agent',
			name: 'worker-agent-test',
			branch: 'main',
			taskState: 'closed',
			sandboxState: 'stopped',
			agentStatus: 'newly_idle',
			agentStatusMessage: 'Stopped recently after running',
			updated: '21m ago',
			provider: 'Firecracker',
			diff: { files: 3, additions: 21, deletions: 118 }
		},
		{
			id: 'sbx-docs-031',
			directory: '/home/discobot/experiments/docs',
			name: 'docs-openapi-pass',
			branch: 'docs/api-examples',
			taskState: 'merged',
			sandboxState: 'stopped',
			agentStatus: 'idle',
			updated: '34m ago',
			provider: 'Docker',
			diff: { files: 6, additions: 152, deletions: 18 }
		}
	];

	const settingsDialogId = 'settings-dialog';
	const activeSandbox = sandboxes[1];
	const themeModes: ThemeMode[] = ['light', 'dark', 'system'];
	const context = setContext(createContext());
	const themeMode = $derived(context.view.app.preferences.theme);
	const colorScheme = $derived(context.view.app.preferences.colorScheme);
	const resolvedTheme = $derived(context.view.app.preferences.resolvedTheme);
	const availableThemes = $derived(context.view.app.preferences.availableThemes);
	const activeTheme = $derived(getThemeMetadata(resolvedTheme, colorScheme));
	const sidebarCollapsed = $derived(!context.view.navigation.desktopSidebarOpen);
	const settingsOpen = $derived(context.view.app.dialogs.settings.open);
	const windowControls = $derived(context.view.app.environment.windowControls);

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

	function setThemeMode(mode: ThemeMode) {
		void context.commands.preferences.setTheme(mode);
	}

	function setColorScheme(scheme: ThemeColorScheme) {
		void context.commands.preferences.setColorScheme(scheme);
	}

	function toggleSidebar() {
		void context.commands.navigation.toggleDesktopSidebarOpen();
	}

	function openSettings() {
		void context.commands.dialogs.openSettingsDialog();
	}

	function closeSettings() {
		void context.commands.dialogs.closeSettingsDialog();
	}

	onMount(() => {
		const environment = getAppEnvironment();

		let unsubscribeWindowControls: (() => void) | undefined;
		void context.commands.environment.hydrateEnvironment();
		void environment.getWindowControls().then((mode) => {
			void context.commands.environment.setWindowControls(mode);
		});
		unsubscribeWindowControls = environment.onWindowControlsChange((mode) => {
			void context.commands.environment.setWindowControls(mode);
		});

		void context.commands.preferences.setTheme(context.view.app.preferences.theme);

		const media = window.matchMedia('(prefers-color-scheme: dark)');
		const handleSystemThemeChange = () => {
			void context.commands.preferences.refreshSystemTheme();
		};

		media.addEventListener('change', handleSystemThemeChange);
		return () => {
			media.removeEventListener('change', handleSystemThemeChange);
			unsubscribeWindowControls?.();
		};
	});
</script>

<svelte:head>
	<title>Discobot Sandbox Mock</title>
</svelte:head>

<div class="flex h-[100dvh] flex-col overflow-hidden bg-background text-foreground">
	<AppHeader
		{sidebarCollapsed}
		{windowControls}
		onSidebarToggle={toggleSidebar}
		onSettingsOpen={openSettings}
		onTitleBarDoubleClick={() => void context.commands.environment.toggleWindowMaximized()}
	/>

	<div class="flex min-h-0 flex-1 overflow-hidden">
		{#if !sidebarCollapsed}
			<SandboxSidebar {activeSandbox} {sandboxes} homeDirectory="/home/discobot" />
		{/if}
		<SandboxWorkspace {activeSandbox} {files} {codeLines} {terminalLines} />
	</div>

	<SettingsDialog
		modalId={settingsDialogId}
		open={settingsOpen}
		{themeModes}
		{themeMode}
		{colorScheme}
		{availableThemes}
		{activeTheme}
		onThemeModeChange={setThemeMode}
		onColorSchemeChange={setColorScheme}
		onClose={closeSettings}
	/>
</div>
