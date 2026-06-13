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
	document.documentElement.removeAttribute('data-theme');
	setSystemTheme(false);
});

afterEach(() => {
	vi.unstubAllGlobals();
});

describe('theme color schemes', () => {
	it('includes all daisyUI built-in themes in the dropdown options', () => {
		const themeIds = getAvailableThemes('light').map((theme) => theme.id);

		expect(themeIds).toEqual([
			'light',
			'dark',
			'cupcake',
			'bumblebee',
			'emerald',
			'corporate',
			'synthwave',
			'retro',
			'cyberpunk',
			'valentine',
			'halloween',
			'garden',
			'forest',
			'aqua',
			'lofi',
			'pastel',
			'fantasy',
			'wireframe',
			'black',
			'luxury',
			'dracula',
			'cmyk',
			'autumn',
			'business',
			'acid',
			'lemonade',
			'night',
			'coffee',
			'winter',
			'dim',
			'nord',
			'sunset',
			'caramellatte',
			'abyss',
			'silk'
		]);
		expect(getAvailableThemes('dark').map((theme) => theme.id)).toEqual(themeIds);
	});

	it('keeps any daisyUI built-in scheme regardless of resolved mode', () => {
		expect(normalizeColorScheme('light', 'dark')).toBe('dark');
		expect(normalizeColorScheme('dark', 'light')).toBe('light');
		expect(normalizeColorScheme('light', 'synthwave')).toBe('synthwave');
		expect(normalizeColorScheme('dark', 'cupcake')).toBe('cupcake');
	});

	it('falls back to the light built-in theme when no stored scheme is available', () => {
		expect(getColorScheme()).toBe('light');
	});

	it('returns metadata for built-in schemes', () => {
		const metadata = getThemeMetadata('dark', 'dracula');

		expect(metadata).toMatchObject({ id: 'dracula', name: 'Dracula', mode: 'dark' });
	});
});

describe('theme preference application', () => {
	it('applies and stores a valid light color scheme in system light mode', () => {
		setSystemTheme(false);

		const preferences = applyThemePreferences('system', 'winter');

		expect(preferences).toMatchObject({
			theme: 'system',
			resolvedTheme: 'light',
			colorScheme: 'winter'
		});
		expect(document.documentElement).not.toHaveClass('dark');
		expect(document.documentElement).toHaveAttribute('data-theme', 'winter');
		expect(localStorage.getItem('theme')).toBe('system');
		expect(localStorage.getItem('theme.colorScheme')).toBe('winter');
	});

	it('allows dark daisyUI schemes in system light mode', () => {
		setSystemTheme(false);

		const preferences = applyThemePreferences('system', 'night');

		expect(preferences).toMatchObject({
			theme: 'system',
			resolvedTheme: 'light',
			colorScheme: 'night'
		});
		expect(document.documentElement).not.toHaveClass('dark');
		expect(document.documentElement).toHaveAttribute('data-theme', 'night');
		expect(localStorage.getItem('theme.colorScheme')).toBe('night');
	});

	it('allows light daisyUI schemes in system dark mode', () => {
		setSystemTheme(true);

		const preferences = applyThemePreferences('system', 'cupcake');

		expect(preferences).toMatchObject({
			theme: 'system',
			resolvedTheme: 'dark',
			colorScheme: 'cupcake'
		});
		expect(document.documentElement).toHaveClass('dark');
		expect(document.documentElement).toHaveAttribute('data-theme', 'cupcake');
		expect(localStorage.getItem('theme.colorScheme')).toBe('cupcake');
	});

	it('applies explicitly selected built-in schemes', () => {
		const scheme: ThemeColorScheme = 'dracula';

		const preferences = applyThemePreferences('dark', scheme);

		expect(preferences.colorScheme).toBe(scheme);
		expect(document.documentElement).toHaveClass('dark');
		expect(document.documentElement).toHaveAttribute('data-theme', scheme);
	});
});
