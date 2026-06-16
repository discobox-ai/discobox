import type { ResolvedTheme, ThemeColorScheme, ThemeMetadata, ThemeMode } from '$lib/theme';

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
};
