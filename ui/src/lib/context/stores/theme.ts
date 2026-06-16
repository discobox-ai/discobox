import { readStorage, writeStorage } from '$lib/local-storage';
import type { ThemeColorScheme, ThemeMode } from '$lib/theme';

const THEME_KEY = 'theme';
const COLOR_SCHEME_KEY = 'theme.colorScheme';

export const themeStore = {
	readThemeMode(): ThemeMode | null {
		const storedTheme = readStorage(THEME_KEY);
		return storedTheme === 'light' || storedTheme === 'dark' || storedTheme === 'system'
			? storedTheme
			: null;
	},
	setThemeMode(theme: ThemeMode): ThemeMode {
		writeStorage(THEME_KEY, theme);
		return theme;
	},
	readColorScheme(): ThemeColorScheme | null {
		const storedScheme = readStorage(COLOR_SCHEME_KEY);
		return storedScheme === 'default' ||
			storedScheme === 'flexoki' ||
			storedScheme === 'nord' ||
			storedScheme === 'tokyo-night' ||
			storedScheme === 'solarized' ||
			storedScheme === 'dracula' ||
			storedScheme === 'catppuccin-mocha' ||
			storedScheme === 'catppuccin-macchiato' ||
			storedScheme === 'catppuccin-frappe' ||
			storedScheme === 'alucard' ||
			storedScheme === 'catppuccin-latte'
			? storedScheme
			: null;
	},
	setColorScheme(scheme: ThemeColorScheme): ThemeColorScheme {
		writeStorage(COLOR_SCHEME_KEY, scheme);
		return scheme;
	}
};
