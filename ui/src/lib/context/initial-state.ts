import type { DataState, ViewState } from '$lib/context/context.types';
import {
	getAvailableThemes,
	getColorScheme,
	getThemeMode,
	normalizeColorScheme,
	resolveThemeMode
} from '$lib/theme';

export function createInitialDataState(): DataState {
	return {
		environment: {
			apiBase: '',
			isDesktop: false,
			platform: 'web'
		}
	};
}

export function createInitialViewState(): ViewState {
	const theme = getThemeMode();
	const resolvedTheme = resolveThemeMode(theme);
	const colorScheme = normalizeColorScheme(resolvedTheme, getColorScheme());

	return {
		app: {
			environment: {
				isMobile: false,
				isMacPlatform: false,
				windowControls: 'none'
			},
			dialogs: {
				settings: {
					open: false
				}
			},
			preferences: {
				theme,
				resolvedTheme,
				colorScheme,
				availableThemes: getAvailableThemes(resolvedTheme)
			}
		},
		navigation: {
			desktopSidebarOpen: true
		}
	};
}
