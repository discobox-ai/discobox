// See https://svelte.dev/docs/kit/types#app.d.ts
// for information about these interfaces
import type { DesktopEnvironmentBridge } from '$lib/environment';

declare module '*.css';

declare global {
	interface Window {
		__DISCOBOX_DESKTOP__?: DesktopEnvironmentBridge;
	}

	namespace App {
		// interface Error {}
		// interface Locals {}
		// interface PageData {}
		// interface PageState {}
		// interface Platform {}
	}
}

export {};
