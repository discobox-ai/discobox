export type Sandbox = {
	id: string;
	directory: string;
	name: string;
	branch: string;
	status: 'running' | 'review' | 'paused';
	updated: string;
	provider: string;
};

export type SandboxGroup = {
	directory: string;
	items: Sandbox[];
};

export type WorkspaceFile = {
	name: string;
	state: string;
};
