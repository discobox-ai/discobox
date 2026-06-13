export type ResolvedTheme = 'dark' | 'light';
export type ThemeMode = ResolvedTheme | 'system';

export type ThemeColorScheme =
	| 'light'
	| 'dark'
	| 'cupcake'
	| 'bumblebee'
	| 'emerald'
	| 'corporate'
	| 'synthwave'
	| 'retro'
	| 'cyberpunk'
	| 'valentine'
	| 'halloween'
	| 'garden'
	| 'forest'
	| 'aqua'
	| 'lofi'
	| 'pastel'
	| 'fantasy'
	| 'wireframe'
	| 'black'
	| 'luxury'
	| 'dracula'
	| 'cmyk'
	| 'autumn'
	| 'business'
	| 'acid'
	| 'lemonade'
	| 'night'
	| 'coffee'
	| 'winter'
	| 'dim'
	| 'nord'
	| 'sunset'
	| 'caramellatte'
	| 'abyss'
	| 'silk';

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

export const THEME_KEY = 'theme';
export const COLOR_SCHEME_KEY = 'theme.colorScheme';

const THEME_METADATA: ThemeMetadata[] = [
	{
		id: 'light',
		name: 'Light',
		mode: 'light',
		preview: {
			background: 'oklch(100% 0 0)',
			primary: 'oklch(45% 0.24 277.023)',
			foreground: 'oklch(21% 0.006 285.885)'
		}
	},
	{
		id: 'dark',
		name: 'Dark',
		mode: 'dark',
		preview: {
			background: 'oklch(25.33% 0.016 252.42)',
			primary: 'oklch(58% 0.233 277.117)',
			foreground: 'oklch(97.807% 0.029 256.847)'
		}
	},
	{
		id: 'cupcake',
		name: 'Cupcake',
		mode: 'light',
		preview: {
			background: 'oklch(97.788% 0.004 56.375)',
			primary: 'oklch(85% 0.138 181.071)',
			foreground: 'oklch(23.574% 0.066 313.189)'
		}
	},
	{
		id: 'bumblebee',
		name: 'Bumblebee',
		mode: 'light',
		preview: {
			background: 'oklch(100% 0 0)',
			primary: 'oklch(85% 0.199 91.936)',
			foreground: 'oklch(20% 0 0)'
		}
	},
	{
		id: 'emerald',
		name: 'Emerald',
		mode: 'light',
		preview: {
			background: 'oklch(100% 0 0)',
			primary: 'oklch(76.662% 0.135 153.45)',
			foreground: 'oklch(35.519% 0.032 262.988)'
		}
	},
	{
		id: 'corporate',
		name: 'Corporate',
		mode: 'light',
		preview: {
			background: 'oklch(100% 0 0)',
			primary: 'oklch(58% 0.158 241.966)',
			foreground: 'oklch(22.389% 0.031 278.072)'
		}
	},
	{
		id: 'synthwave',
		name: 'Synthwave',
		mode: 'dark',
		preview: {
			background: 'oklch(15% 0.09 281.288)',
			primary: 'oklch(71% 0.202 349.761)',
			foreground: 'oklch(78% 0.115 274.713)'
		}
	},
	{
		id: 'retro',
		name: 'Retro',
		mode: 'light',
		preview: {
			background: 'oklch(91.637% 0.034 90.515)',
			primary: 'oklch(80% 0.114 19.571)',
			foreground: 'oklch(41% 0.112 45.904)'
		}
	},
	{
		id: 'cyberpunk',
		name: 'Cyberpunk',
		mode: 'light',
		preview: {
			background: 'oklch(94.51% 0.179 104.32)',
			primary: 'oklch(74.22% 0.209 6.35)',
			foreground: 'oklch(0% 0 0)'
		}
	},
	{
		id: 'valentine',
		name: 'Valentine',
		mode: 'light',
		preview: {
			background: 'oklch(97% 0.014 343.198)',
			primary: 'oklch(65% 0.241 354.308)',
			foreground: 'oklch(52% 0.223 3.958)'
		}
	},
	{
		id: 'halloween',
		name: 'Halloween',
		mode: 'dark',
		preview: {
			background: 'oklch(21% 0.006 56.043)',
			primary: 'oklch(77.48% 0.204 60.62)',
			foreground: 'oklch(84.955% 0 0)'
		}
	},
	{
		id: 'garden',
		name: 'Garden',
		mode: 'light',
		preview: {
			background: 'oklch(92.951% 0.002 17.197)',
			primary: 'oklch(62.45% 0.278 3.836)',
			foreground: 'oklch(16.961% 0.001 17.32)'
		}
	},
	{
		id: 'forest',
		name: 'Forest',
		mode: 'dark',
		preview: {
			background: 'oklch(20.84% 0.008 17.911)',
			primary: 'oklch(68.628% 0.185 148.958)',
			foreground: 'oklch(83.768% 0.001 17.911)'
		}
	},
	{
		id: 'aqua',
		name: 'Aqua',
		mode: 'dark',
		preview: {
			background: 'oklch(37% 0.146 265.522)',
			primary: 'oklch(85.661% 0.144 198.645)',
			foreground: 'oklch(90% 0.058 230.902)'
		}
	},
	{
		id: 'lofi',
		name: 'Lofi',
		mode: 'light',
		preview: {
			background: 'oklch(100% 0 0)',
			primary: 'oklch(15.906% 0 0)',
			foreground: 'oklch(0% 0 0)'
		}
	},
	{
		id: 'pastel',
		name: 'Pastel',
		mode: 'light',
		preview: {
			background: 'oklch(100% 0 0)',
			primary: 'oklch(90% 0.063 306.703)',
			foreground: 'oklch(20% 0 0)'
		}
	},
	{
		id: 'fantasy',
		name: 'Fantasy',
		mode: 'light',
		preview: {
			background: 'oklch(100% 0 0)',
			primary: 'oklch(37.45% 0.189 325.02)',
			foreground: 'oklch(27.807% 0.029 256.847)'
		}
	},
	{
		id: 'wireframe',
		name: 'Wireframe',
		mode: 'light',
		preview: {
			background: 'oklch(100% 0 0)',
			primary: 'oklch(87% 0 0)',
			foreground: 'oklch(20% 0 0)'
		}
	},
	{
		id: 'black',
		name: 'Black',
		mode: 'dark',
		preview: {
			background: 'oklch(0% 0 0)',
			primary: 'oklch(35% 0 0)',
			foreground: 'oklch(87.609% 0 0)'
		}
	},
	{
		id: 'luxury',
		name: 'Luxury',
		mode: 'dark',
		preview: {
			background: 'oklch(14.076% 0.004 285.822)',
			primary: 'oklch(100% 0 0)',
			foreground: 'oklch(75.687% 0.123 76.89)'
		}
	},
	{
		id: 'dracula',
		name: 'Dracula',
		mode: 'dark',
		preview: {
			background: 'oklch(28.822% 0.022 277.508)',
			primary: 'oklch(75.461% 0.183 346.812)',
			foreground: 'oklch(97.747% 0.007 106.545)'
		}
	},
	{
		id: 'cmyk',
		name: 'CMYK',
		mode: 'light',
		preview: {
			background: 'oklch(100% 0 0)',
			primary: 'oklch(71.772% 0.133 239.443)',
			foreground: 'oklch(20% 0 0)'
		}
	},
	{
		id: 'autumn',
		name: 'Autumn',
		mode: 'light',
		preview: {
			background: 'oklch(95.814% 0 0)',
			primary: 'oklch(40.723% 0.161 17.53)',
			foreground: 'oklch(19.162% 0 0)'
		}
	},
	{
		id: 'business',
		name: 'Business',
		mode: 'dark',
		preview: {
			background: 'oklch(24.353% 0 0)',
			primary: 'oklch(41.703% 0.099 251.473)',
			foreground: 'oklch(84.87% 0 0)'
		}
	},
	{
		id: 'acid',
		name: 'Acid',
		mode: 'light',
		preview: {
			background: 'oklch(98% 0 0)',
			primary: 'oklch(71.9% 0.357 330.759)',
			foreground: 'oklch(0% 0 0)'
		}
	},
	{
		id: 'lemonade',
		name: 'Lemonade',
		mode: 'light',
		preview: {
			background: 'oklch(98.71% 0.02 123.72)',
			primary: 'oklch(58.92% 0.199 134.6)',
			foreground: 'oklch(19.742% 0.004 123.72)'
		}
	},
	{
		id: 'night',
		name: 'Night',
		mode: 'dark',
		preview: {
			background: 'oklch(20.768% 0.039 265.754)',
			primary: 'oklch(75.351% 0.138 232.661)',
			foreground: 'oklch(84.153% 0.007 265.754)'
		}
	},
	{
		id: 'coffee',
		name: 'Coffee',
		mode: 'dark',
		preview: {
			background: 'oklch(24% 0.023 329.708)',
			primary: 'oklch(71.996% 0.123 62.756)',
			foreground: 'oklch(72.354% 0.092 79.129)'
		}
	},
	{
		id: 'winter',
		name: 'Winter',
		mode: 'light',
		preview: {
			background: 'oklch(100% 0 0)',
			primary: 'oklch(56.86% 0.255 257.57)',
			foreground: 'oklch(41.886% 0.053 255.824)'
		}
	},
	{
		id: 'dim',
		name: 'Dim',
		mode: 'dark',
		preview: {
			background: 'oklch(30.857% 0.023 264.149)',
			primary: 'oklch(86.133% 0.141 139.549)',
			foreground: 'oklch(82.901% 0.031 222.959)'
		}
	},
	{
		id: 'nord',
		name: 'Nord',
		mode: 'light',
		preview: {
			background: 'oklch(95.127% 0.007 260.731)',
			primary: 'oklch(59.435% 0.077 254.027)',
			foreground: 'oklch(32.437% 0.022 264.182)'
		}
	},
	{
		id: 'sunset',
		name: 'Sunset',
		mode: 'dark',
		preview: {
			background: 'oklch(22% 0.019 237.69)',
			primary: 'oklch(74.703% 0.158 39.947)',
			foreground: 'oklch(77.383% 0.043 245.096)'
		}
	},
	{
		id: 'caramellatte',
		name: 'Caramellatte',
		mode: 'light',
		preview: {
			background: 'oklch(98% 0.016 73.684)',
			primary: 'oklch(0% 0 0)',
			foreground: 'oklch(40% 0.123 38.172)'
		}
	},
	{
		id: 'abyss',
		name: 'Abyss',
		mode: 'dark',
		preview: {
			background: 'oklch(20% 0.08 209)',
			primary: 'oklch(92% 0.2653 125)',
			foreground: 'oklch(90% 0.076 70.697)'
		}
	},
	{
		id: 'silk',
		name: 'Silk',
		mode: 'light',
		preview: {
			background: 'oklch(97% 0.0035 67.78)',
			primary: 'oklch(23.27% 0.0249 284.3)',
			foreground: 'oklch(40% 0.0081 61.42)'
		}
	}
];

function readStorage(key: string): string | null {
	if (typeof localStorage === 'undefined') return null;
	return localStorage.getItem(key);
}

function writeStorage(key: string, value: string) {
	if (typeof localStorage === 'undefined') return;
	localStorage.setItem(key, value);
}

export function resolvePreferredTheme(): ResolvedTheme {
	if (typeof window === 'undefined' || typeof window.matchMedia !== 'function') return 'dark';
	return window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
}

export function resolveThemeMode(theme: ThemeMode): ResolvedTheme {
	return theme === 'system' ? resolvePreferredTheme() : theme;
}

export function getThemeMode(): ThemeMode {
	const storedTheme = readStorage(THEME_KEY);
	return storedTheme === 'light' || storedTheme === 'dark' || storedTheme === 'system'
		? storedTheme
		: 'system';
}

export function getColorScheme(): ThemeColorScheme {
	const storedScheme = readStorage(COLOR_SCHEME_KEY);
	return isThemeColorScheme(storedScheme) ? storedScheme : 'light';
}

export function getAvailableThemes(mode: ResolvedTheme): ThemeMetadata[] {
	void mode;
	return THEME_METADATA;
}

export function getThemeMetadata(mode: ResolvedTheme, scheme: ThemeColorScheme): ThemeMetadata {
	return (
		THEME_METADATA.find((theme) => theme.id === scheme) ??
		THEME_METADATA.find((theme) => theme.id === (mode === 'dark' ? 'dark' : 'light')) ??
		THEME_METADATA[0]
	);
}

export function normalizeColorScheme(
	mode: ResolvedTheme,
	scheme: ThemeColorScheme
): ThemeColorScheme {
	void mode;
	return isThemeColorScheme(scheme) ? scheme : 'light';
}

export function isThemeColorScheme(value: string | null): value is ThemeColorScheme {
	return THEME_METADATA.some((theme) => theme.id === value);
}

export function applyTheme(theme: ThemeMode): ThemeMode {
	if (typeof document === 'undefined') return theme;
	const resolved = resolveThemeMode(theme);
	document.documentElement.classList.toggle('dark', resolved === 'dark');
	writeStorage(THEME_KEY, theme);
	return theme;
}

export function applyColorScheme(scheme: ThemeColorScheme): ThemeColorScheme {
	if (typeof document === 'undefined') return scheme;
	document.documentElement.setAttribute('data-theme', scheme);
	writeStorage(COLOR_SCHEME_KEY, scheme);
	return scheme;
}

export function applyThemePreferences(theme: ThemeMode, scheme: ThemeColorScheme) {
	applyTheme(theme);
	const resolvedTheme = resolveThemeMode(theme);
	const normalizedScheme = normalizeColorScheme(resolvedTheme, scheme);
	applyColorScheme(normalizedScheme);
	return {
		theme,
		resolvedTheme,
		colorScheme: normalizedScheme
	};
}
