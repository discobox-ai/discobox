---
name: UI design system review
type: file
engine: ai
pattern: "ui/**/*.{css,svelte,ts,json}"
description: Review UI changes for shadcn-svelte design-system consistency, Discobot visual style, and theme safety
phase: review
---
Review the changed UI CSS, Svelte, TypeScript, and shadcn configuration files for design-system and visual-style consistency.

Before reviewing, use the local repository guidance as the source of truth:

- `DESIGN.md` for the repository-level UI design-system direction.
- `ui/DESIGN.md` for UI package-specific shadcn-svelte guidance.
- `ui/components.json` for shadcn-svelte registry configuration.
- `ui/src/app.css` for the Discobot shadcn token/theme layer.

Focus on whether the changes align with the Discobot UI direction established in this workspace:

1. shadcn-svelte/Tailwind setup
   - Tailwind CSS v4 and shadcn-svelte setup should stay CSS-first through `ui/src/app.css` and `ui/components.json`.
   - The expected base setup uses Tailwind, shadcn-svelte component styles, and the Discobot token layer in `ui/src/app.css`.
   - Prefer generated shadcn-svelte primitives from `ui/src/lib/components/ui` for reusable controls and surfaces.
   - Keep dependencies, component classes, and theme configuration aligned with shadcn-svelte and the Discobot token layer.
   - Do not add `tailwind.config.js` unless there is a clear product need that cannot be represented in the existing Tailwind CSS v4 setup.

2. Theme safety and semantic colors
   - Prefer shadcn/Discobot token utilities such as `bg-background`, `text-foreground`, `border-border`, `bg-card`, `text-card-foreground`, `bg-muted`, `text-muted-foreground`, `bg-primary`, `text-primary-foreground`, and `text-destructive`.
   - Use Discobot app tokens from `ui/src/app.css` where appropriate, such as `bg-tree-hover`, `bg-tree-selected`, `bg-terminal-bg`, `text-terminal-fg`, and diff tokens.
   - Theme state should use the Discobot shadcn model: `.dark`, `data-theme`, and CSS variables from `ui/src/app.css`.
   - Do not hardcode dark-only colors for structural UI, especially headers, sidebars, cards, panels, borders, and text. Hardcoded colors are acceptable only for intentionally fixed brand assets, macOS traffic-light controls, syntax/code mock content, or explicitly decorative details.
   - Verify changes work across configured light and dark Discobot theme variants and do not rely on a single fixed palette.

3. shadcn-svelte usage
   - Prefer shadcn-svelte component primitives and Tailwind utilities over custom CSS. New custom CSS should be minimal, app-specific, and placed where it belongs.
   - Use existing generated components from `ui/src/lib/components/ui` before adding new primitives or custom component APIs.
   - If a generated shadcn component is modified, keep the change small, explainable, and compatible with the rest of the generated registry.
   - Tailwind utility overrides are acceptable when shadcn primitives are not enough. Avoid `!` important utilities unless specificity makes them necessary.
   - Only use real shadcn component exports, Discobot tokens, or Tailwind utility classes. Avoid invented component or modifier names.

4. Typography
   - UI text should use the Geist Sans stack configured in `ui/src/app.css`; code and terminal content should use JetBrains Mono or the existing `.mono` helper.
   - Do not add new custom fonts unless they are necessary for the product direction.
   - Toolbar and compact controls should generally match Discobot's compact scale: `text-xs`, `font-medium`, `h-6`, tight gaps, and lucide icons around `size-3` to `size-3.5`.
   - Avoid unnecessarily heavy weights such as `font-bold`/`font-semibold` in dense chrome unless there is a clear hierarchy need.
   - Avoid excessive uppercase/tracking for ordinary actions. Reserve uppercase/tracked text for section labels or metadata.

5. Discobot visual style
   - Prefer compact, flat, icon-plus-text controls over large pill-like chrome in app toolbar/header areas.
   - Use lucide icons from `@lucide/svelte/icons/...` when adding toolbar/action icons.
   - Keep header, toolbar, sidebar, editor, and terminal treatments cohesive with the upstream Discobot look and the current mock components.
   - Preserve Electron-friendly drag/no-drag behavior when changing header/window chrome: draggable regions should keep `desktop-drag-region`; interactive controls should keep `desktop-no-drag`.
   - Keep the full-width top header and custom shell layout unless the product direction explicitly changes.

6. Accessibility and maintainability
   - Interactive icon-only controls need accessible names (`aria-label` or visible text).
   - Decorative mock controls should not create misleading functional behavior.
   - Keep responsive `flex` and `grid` layouts usable across expected window sizes.
   - Keep component markup readable and avoid broad refactors unrelated to the design change.
   - Keep UI interactions local and mock-driven unless backend integration is explicitly requested.

Only provide feedback for concrete issues introduced by the changed files. If a change is acceptable but could be a future improvement, do not block on it unless it conflicts with the design system above.
