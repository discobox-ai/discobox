import type { ResolvedTheme, ThemeColorScheme, ThemeMetadata, ThemeMode } from '$lib/theme';
import type { AppPlatform, WindowControlsMode } from '$lib/environment';

export type CommandOptions = {
	wait?: boolean;
};

export type Context = {
	data: DataState;
	view: ViewState;
	commands: Commands;
};

export type DataState = {
	environment: {
		apiBase: string;
		isDesktop: boolean;
		platform: AppPlatform;
	};
};

export type ViewState = {
	app: AppViewState;
	navigation: NavigationViewState;
};

export type AppViewState = {
	environment: {
		isMobile: boolean;
		isMacPlatform: boolean;
		windowControls: WindowControlsMode;
	};
	dialogs: {
		settings: {
			open: boolean;
		};
	};
	preferences: {
		theme: ThemeMode;
		resolvedTheme: ResolvedTheme;
		colorScheme: ThemeColorScheme;
		availableThemes: ThemeMetadata[];
	};
};

export type NavigationViewState = {
	desktopSidebarOpen: boolean;
};

export type Commands = {
	dialogs: {
		setSettingsDialogOpen(open: boolean, options?: CommandOptions): Promise<void>;
		openSettingsDialog(options?: CommandOptions): Promise<void>;
		closeSettingsDialog(options?: CommandOptions): Promise<void>;
	};
	navigation: {
		setDesktopSidebarOpen(open: boolean, options?: CommandOptions): Promise<void>;
		toggleDesktopSidebarOpen(options?: CommandOptions): Promise<void>;
	};
	preferences: {
		setTheme(theme: ThemeMode, options?: CommandOptions): Promise<void>;
		setColorScheme(scheme: ThemeColorScheme, options?: CommandOptions): Promise<void>;
		refreshSystemTheme(options?: CommandOptions): Promise<void>;
	};
	environment: {
		hydrateEnvironment(options?: CommandOptions): Promise<void>;
		setWindowControls(windowControls: WindowControlsMode, options?: CommandOptions): Promise<void>;
		toggleWindowMaximized(options?: CommandOptions): Promise<void>;
	};
};
