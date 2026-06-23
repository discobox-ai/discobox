import { getContext as getSvelteContext, setContext as setSvelteContext } from 'svelte';

import { createCommands } from '$lib/context/commands';
import type { Context } from '$lib/context/context.types';
import { createInitialDataState, createInitialViewState } from '$lib/context/initial-state';

const CONTEXT_KEY = Symbol('discobot-context');

let currentContext: Context | null = null;

export function createContext(): Context {
	const context = $state<Context>({
		data: createInitialDataState(),
		view: createInitialViewState(),
		commands: undefined as unknown as Context['commands']
	});

	context.commands = createCommands(context);
	void context.commands.environment.hydrateEnvironment();
	currentContext = context;

	return context;
}

export function setContext(context: Context): Context {
	currentContext = context;
	setSvelteContext(CONTEXT_KEY, context);
	return context;
}

export function getContext(): Context {
	if (!currentContext) {
		throw new Error('context has not been created');
	}
	return currentContext;
}

export function useContext(): Context {
	return getSvelteContext<Context>(CONTEXT_KEY);
}
