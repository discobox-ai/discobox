import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
	applyThemePreferences,
	getAvailableThemes,
	getColorScheme,
	getThemeMetadata,
	normalizeColorScheme,
	type ThemeColorScheme
} from './theme';

function createLocalStorageMock(): Storage {
	const values = new Map<string, string>();

	return {
		get length() {
			return values.size;
		},
		clear: vi.fn(() => values.clear()),
		getItem: vi.fn((key: string) => values.get(key) ?? null),
		key: vi.fn((index: number) => Array.from(values.keys())[index] ?? null),
		removeItem: vi.fn((key: string) => values.delete(key)),
		setItem: vi.fn((key: string, value: string) => values.set(key, value))
	};
}

function setSystemTheme(prefersDark: boolean) {
	vi.stubGlobal(
		'matchMedia',
		vi.fn().mockImplementation((query: string) => ({
			matches: query === '(prefers-color-scheme: dark)' ? prefersDark : false,
			media: query,
			onchange: null,
			addEventListener: vi.fn(),
			removeEventListener: vi.fn(),
			addListener: vi.fn(),
			removeListener: vi.fn(),
			dispatchEvent: vi.fn()
		}))
	);
}

beforeEach(() => {
	vi.stubGlobal('localStorage', createLocalStorageMock());
	localStorage.clear();
	document.documentElement.className = '';
	document.documentElement.removeAttribute('style');
	document.documentElement.removeAttribute('data-theme');
	setSystemTheme(false);
});

afterEach(() => {
	vi.unstubAllGlobals();
});

describe('theme color schemes', () => {
	it('matches the upstream Discobot light theme options', () => {
		expect(getAvailableThemes('light').map((theme) => theme.id)).toEqual([
			'default',
			'solarized',
			'flexoki',
			'alucard',
			'catppuccin-latte'
		]);
	});

	it('matches the upstream Discobot dark theme options', () => {
		expect(getAvailableThemes('dark').map((theme) => theme.id)).toEqual([
			'default',
			'flexoki',
			'nord',
			'tokyo-night',
			'dracula',
			'catppuccin-mocha',
			'catppuccin-macchiato',
			'catppuccin-frappe'
		]);
	});

	it('normalizes schemes against the resolved mode', () => {
		expect(normalizeColorScheme('light', 'solarized')).toBe('solarized');
		expect(normalizeColorScheme('dark', 'tokyo-night')).toBe('tokyo-night');
		expect(normalizeColorScheme('light', 'tokyo-night')).toBe('default');
		expect(normalizeColorScheme('dark', 'catppuccin-latte')).toBe('default');
	});

	it('falls back to the default scheme when no stored scheme is available', () => {
		expect(getColorScheme()).toBe('default');
	});

	it('returns metadata for upstream schemes', () => {
		const metadata = getThemeMetadata('dark', 'tokyo-night');

		expect(metadata).toMatchObject({ id: 'tokyo-night', name: 'Tokyo Night', mode: 'dark' });
	});
});

describe('theme preference application', () => {
	it('applies and stores a valid color scheme in system light mode', () => {
		setSystemTheme(false);

		const preferences = applyThemePreferences('system', 'solarized');

		expect(preferences).toMatchObject({
			theme: 'system',
			resolvedTheme: 'light',
			colorScheme: 'solarized'
		});
		expect(document.documentElement).not.toHaveClass('dark');
		expect(document.documentElement).toHaveAttribute('data-theme', 'solarized');
		expect(localStorage.getItem('theme')).toBe('system');
		expect(localStorage.getItem('theme.colorScheme')).toBe('solarized');
	});

	it('applies the dark class and data-theme in system dark mode', () => {
		setSystemTheme(true);

		const preferences = applyThemePreferences('system', 'tokyo-night');

		expect(preferences).toMatchObject({
			theme: 'system',
			resolvedTheme: 'dark',
			colorScheme: 'tokyo-night'
		});
		expect(document.documentElement).toHaveClass('dark');
		expect(document.documentElement).toHaveAttribute('data-theme', 'tokyo-night');
		expect(localStorage.getItem('theme.colorScheme')).toBe('tokyo-night');
	});

	it('falls back to default when a scheme does not exist for the resolved mode', () => {
		const scheme: ThemeColorScheme = 'catppuccin-latte';

		const preferences = applyThemePreferences('dark', scheme);

		expect(preferences.colorScheme).toBe('default');
		expect(document.documentElement).toHaveClass('dark');
		expect(document.documentElement).toHaveAttribute('data-theme', 'default');
	});
});
