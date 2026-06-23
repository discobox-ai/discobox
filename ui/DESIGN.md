# UI Design

The UI package is a SvelteKit static prototype for the Discobot sandbox shell.
It should remain mock/static unless a task explicitly asks for backend
integration.

## Design System

Use shadcn-svelte as the UI design system.

- Install and maintain generated shadcn-svelte components under
  `src/lib/components/ui`.
- Prefer shadcn-svelte primitives for reusable controls and surfaces, including
  buttons, badges, cards, dialogs, inputs, selects, separators, tabs, dropdowns,
  tables, tooltips, sheets, popovers, command palettes, and menus.
- Use shadcn token utilities such as `bg-background`, `text-foreground`,
  `bg-card`, `text-card-foreground`, `border-border`, `bg-muted`,
  `text-muted-foreground`, `bg-primary`, `text-primary-foreground`, and
  `text-destructive`.
- Theme changes should use the Discobot shadcn-svelte theme model: toggle
  `.dark`, set `data-theme`, and rely on CSS variables in `src/app.css`.
- New UI work should use shadcn-svelte components and token utilities from this
  package.

## Context Architecture

Local UI state follows the upstream Discobot context shape: durable/resource-like
state belongs under `context.data`, transient presentation state belongs under
`context.view`, and mutations are exposed through grouped `context.commands`.
Keep direct component-local state limited to purely internal widget details.
Theme preferences should flow through `context.view.app.preferences` and
`context.commands.preferences`, with persistence delegated to
`src/lib/context/stores/theme.ts`.

## Shell Layout

The prototype shell uses a full-width top header above a collapsible sidebar and
workspace content area. Avoid canonical drawer layouts that constrain the header;
the header should continue spanning the full viewport width.

## Prototype Boundary

Keep UI interactions local and mock-driven unless backend integration is
explicitly requested. Static data is preferred for design exploration.

## Storybook

Use Storybook for isolated component development when a component benefits from
focused visual iteration outside the full shell. Stories should keep component
APIs explicit and should prefer Storybook controls for variant props instead of
duplicating one story per simple option.

Story files should render the component directly by default. Do not add wrapper
elements that constrain width, height, overflow, or layout unless that specific
story is intentionally demonstrating a constrained container.

Use `layout: 'fullscreen'` when a component should be allowed to occupy the full
Storybook preview area. Use Storybook viewport controls for broad viewport
testing, and add story-specific framing only when the container is part of the
scenario under test.
