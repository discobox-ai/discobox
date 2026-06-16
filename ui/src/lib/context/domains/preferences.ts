import type { Context } from '$lib/context/context.types';
import {
	applyColorScheme,
	applyTheme,
	getAvailableThemes,
	normalizeColorScheme,
	resolveThemeMode,
	type ThemeColorScheme,
	type ThemeMode
} from '$lib/theme';

export function setTheme(context: Context, theme: ThemeMode): void {
	const appliedTheme = applyTheme(theme);
	const resolvedTheme = resolveThemeMode(appliedTheme);
	const colorScheme = normalizeColorScheme(resolvedTheme, context.view.app.preferences.colorScheme);
	const appliedColorScheme = applyColorScheme(colorScheme);

	context.view.app.preferences.theme = appliedTheme;
	context.view.app.preferences.resolvedTheme = resolvedTheme;
	context.view.app.preferences.colorScheme = appliedColorScheme;
	context.view.app.preferences.availableThemes = getAvailableThemes(resolvedTheme);
}

export function setColorScheme(context: Context, scheme: ThemeColorScheme): void {
	const resolvedTheme = context.view.app.preferences.resolvedTheme;
	const colorScheme = normalizeColorScheme(resolvedTheme, scheme);
	const appliedColorScheme = applyColorScheme(colorScheme);

	context.view.app.preferences.colorScheme = appliedColorScheme;
}

export function refreshSystemTheme(context: Context): void {
	if (context.view.app.preferences.theme === 'system') {
		setTheme(context, 'system');
	}
}
