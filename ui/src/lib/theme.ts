import { themeStore } from '$lib/context/stores/theme';

export type ResolvedTheme = 'dark' | 'light';
export type ThemeMode = ResolvedTheme | 'system';

export type ThemeColorScheme =
	| 'default'
	| 'flexoki'
	| 'nord'
	| 'tokyo-night'
	| 'solarized'
	| 'dracula'
	| 'catppuccin-mocha'
	| 'catppuccin-macchiato'
	| 'catppuccin-frappe'
	| 'alucard'
	| 'catppuccin-latte';

export type ThemeMetadata = {
	id: ThemeColorScheme;
	name: string;
	mode: ResolvedTheme;
	preview: {
		background: string;
		primary: string;
		foreground: string;
	};
};

const THEME_METADATA: ThemeMetadata[] = [
	{
		id: 'default',
		name: 'Default',
		mode: 'light',
		preview: {
			background: '#fafafa',
			primary: '#4f7ef3',
			foreground: '#262626'
		}
	},
	{
		id: 'solarized',
		name: 'Solarized',
		mode: 'light',
		preview: {
			background: '#fdf6e3',
			primary: '#268bd2',
			foreground: '#657b83'
		}
	},
	{
		id: 'flexoki',
		name: 'Flexoki',
		mode: 'light',
		preview: {
			background: '#fffcf0',
			primary: '#4385be',
			foreground: '#100f0f'
		}
	},
	{
		id: 'alucard',
		name: 'Alucard',
		mode: 'light',
		preview: {
			background: '#fffbeb',
			primary: '#644ac9',
			foreground: '#1f1f1f'
		}
	},
	{
		id: 'catppuccin-latte',
		name: 'Catppuccin Latte',
		mode: 'light',
		preview: {
			background: '#eff1f5',
			primary: '#1e66f5',
			foreground: '#4c4f69'
		}
	},
	{
		id: 'default',
		name: 'Default',
		mode: 'dark',
		preview: {
			background: '#1e1e1e',
			primary: '#4f7ef3',
			foreground: '#ededed'
		}
	},
	{
		id: 'flexoki',
		name: 'Flexoki',
		mode: 'dark',
		preview: {
			background: '#1c1b1a',
			primary: '#205ea6',
			foreground: '#cecdc3'
		}
	},
	{
		id: 'nord',
		name: 'Nord',
		mode: 'dark',
		preview: {
			background: '#2e3440',
			primary: '#88c0d0',
			foreground: '#d8dee9'
		}
	},
	{
		id: 'tokyo-night',
		name: 'Tokyo Night',
		mode: 'dark',
		preview: {
			background: '#1a1b26',
			primary: '#7aa2f7',
			foreground: '#a9b1d6'
		}
	},
	{
		id: 'dracula',
		name: 'Dracula',
		mode: 'dark',
		preview: {
			background: '#282a36',
			primary: '#bd93f9',
			foreground: '#f8f8f2'
		}
	},
	{
		id: 'catppuccin-mocha',
		name: 'Catppuccin Mocha',
		mode: 'dark',
		preview: {
			background: '#1e1e2e',
			primary: '#89b4fa',
			foreground: '#cdd6f4'
		}
	},
	{
		id: 'catppuccin-macchiato',
		name: 'Catppuccin Macchiato',
		mode: 'dark',
		preview: {
			background: '#24273a',
			primary: '#8aadf4',
			foreground: '#cad3f5'
		}
	},
	{
		id: 'catppuccin-frappe',
		name: 'Catppuccin Frappé',
		mode: 'dark',
		preview: {
			background: '#303446',
			primary: '#8caaee',
			foreground: '#c6d0f5'
		}
	}
];

export function resolveThemeMode(theme: ThemeMode): ResolvedTheme {
	if (theme !== 'system') return theme;
	if (typeof window === 'undefined' || typeof window.matchMedia !== 'function') return 'dark';
	return window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
}

export function isThemeMode(value: string | null): value is ThemeMode {
	return value === 'light' || value === 'dark' || value === 'system';
}

export function getThemeMode(): ThemeMode {
	return themeStore.readThemeMode() ?? 'system';
}

export function isThemeColorScheme(value: string | null): value is ThemeColorScheme {
	return THEME_METADATA.some((theme) => theme.id === value);
}

export function getColorScheme(): ThemeColorScheme {
	return themeStore.readColorScheme() ?? 'default';
}

export function getAvailableThemes(mode: ResolvedTheme): ThemeMetadata[] {
	return THEME_METADATA.filter((theme) => theme.mode === mode);
}

export function normalizeColorScheme(
	mode: ResolvedTheme,
	scheme: ThemeColorScheme
): ThemeColorScheme {
	return getAvailableThemes(mode).some((theme) => theme.id === scheme) ? scheme : 'default';
}

export function getThemeMetadata(mode: ResolvedTheme, scheme: ThemeColorScheme): ThemeMetadata {
	const normalizedScheme = normalizeColorScheme(mode, scheme);
	return (
		THEME_METADATA.find((theme) => theme.mode === mode && theme.id === normalizedScheme) ??
		getAvailableThemes(mode)[0]
	);
}

export function applyTheme(theme: ThemeMode): ThemeMode {
	if (typeof document === 'undefined') return theme;
	const resolved = resolveThemeMode(theme);
	document.documentElement.classList.toggle('dark', resolved === 'dark');
	return themeStore.setThemeMode(theme);
}

export function applyColorScheme(
	scheme: ThemeColorScheme,
	mode: ResolvedTheme = resolveThemeMode(getThemeMode())
): ThemeColorScheme {
	if (typeof document === 'undefined') return scheme;
	const normalizedScheme = normalizeColorScheme(mode, scheme);
	document.documentElement.setAttribute('data-theme', normalizedScheme);
	return themeStore.setColorScheme(normalizedScheme);
}

export function applyThemePreferences(theme: ThemeMode, scheme: ThemeColorScheme) {
	applyTheme(theme);
	const resolvedTheme = resolveThemeMode(theme);
	const normalizedScheme = normalizeColorScheme(resolvedTheme, scheme);
	applyColorScheme(normalizedScheme, resolvedTheme);
	return {
		theme,
		resolvedTheme,
		colorScheme: normalizedScheme
	};
}
