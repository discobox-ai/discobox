---
name: UI design system review
type: file
engine: ai
pattern: "ui/**/*.{css,svelte}"
description: Review UI CSS and Svelte changes for daisyUI-first design-system consistency, Discobot visual style, and theme safety
phase: review
---
Review the changed UI CSS and Svelte files for design-system and visual-style consistency.

Before reviewing, load the base `daisyui` skill. Use it as the source of truth for daisyUI 5/Tailwind CSS 4 setup, usage, colors, and component selection. Then read narrower local skill docs as needed:

- `.agents/skills/daisyui/usage/SKILL.md` and `.agents/skills/daisyui/colors/SKILL.md` for any class/color/theme review.
- `.agents/skills/daisyui/config/SKILL.md` for changes to `ui/src/app.css` daisyUI configuration.
- Relevant `.agents/skills/daisyui/components/*.md` files when changed markup adds or materially changes daisyUI components.

Focus on whether the changes align with the Discobot/Discobox UI direction established in this workspace:

1. daisyUI/Tailwind setup
   - Tailwind CSS v4 and daisyUI v5 configuration should live in CSS. Do not introduce `tailwind.config.js` for daisyUI/Tailwind configuration.
   - The expected base setup is `@import 'tailwindcss';` plus `@plugin 'daisyui';` or the existing configured form in `ui/src/app.css`.
   - Keep built-in daisyUI themes enabled through the existing `themes: all;` config unless the product requirement changes.
   - Do not duplicate built-in daisyUI theme definitions or reintroduce removed custom theme blocks.
   - Do not add a daisyUI class prefix or include/exclude config unless the related markup and review guidance are updated consistently.

2. Theme safety and semantic colors
   - Prefer semantic daisyUI/Tailwind theme utilities such as `bg-base-100`, `bg-base-200`, `bg-base-300`, `text-base-content`, `border-base-300`, `text-primary`, `bg-primary`, `text-success`, `text-error`, `bg-neutral`, and matching `*-content` colors.
   - Avoid shadcn-style or app-specific color alias variables such as `--background`, `--foreground`, `--card`, `--muted`, `--accent`, `--sidebar`, or `--terminal-*`. Use daisyUI tokens/utilities directly unless a genuinely app-specific global token is required.
   - Do not hardcode dark-only colors for structural UI, especially headers, sidebars, cards, panels, borders, and text. Hardcoded colors are acceptable only for intentionally fixed brand assets, macOS traffic-light controls, syntax/code mock content, or other explicitly decorative details.
   - Verify changes work across built-in light and dark daisyUI themes and do not rely on `.dark` unless scoped to the correct theme selector.

3. daisyUI usage
   - Prefer daisyUI component classes and Tailwind utilities over custom CSS. New custom CSS should be minimal, app-specific, and placed where it belongs.
   - Use the default variant of daisyUI components unless the user asks for a specific variant/color or the existing Discobox visual style requires one.
   - Tailwind utility overrides are acceptable when daisyUI classes are not enough. Avoid `!` important utilities unless specificity makes them necessary.
   - Only use real daisyUI class names or Tailwind utility classes. Avoid invented component or modifier names.
   - When adding or changing daisyUI components, follow the local daisyUI skill guidance and review the relevant component docs under `.agents/skills/daisyui/components/`.

4. Typography
   - UI text should use the Geist Sans stack configured in `ui/src/app.css`; code and terminal content should use JetBrains Mono or the existing `.mono` helper.
   - Do not add new custom fonts unless they are necessary for the product direction. daisyUI built-in themes should not be expected to provide font families.
   - Toolbar and compact controls should generally match Discobot's compact scale: `text-xs`, `font-medium`, `h-6`, tight gaps, and lucide icons around `size-3` to `size-3.5`.
   - Avoid unnecessarily heavy weights such as `font-bold`/`font-semibold` in dense chrome unless there is a clear hierarchy need.
   - Avoid excessive uppercase/tracking for ordinary actions. Reserve uppercase/tracked text for section labels or metadata.

5. Discobot visual style
   - Prefer compact, flat, icon-plus-text controls over large pill-like chrome in app toolbar/header areas.
   - Use lucide icons from `@lucide/svelte/icons/...` when adding toolbar/action icons.
   - Keep header, toolbar, sidebar, editor, and terminal treatments cohesive with the existing mock components.
   - Preserve Electron-friendly drag/no-drag behavior when changing header/window chrome: draggable regions should keep `desktop-drag-region`; interactive controls should keep `desktop-no-drag`.

6. Accessibility and maintainability
   - Interactive icon-only controls need accessible names (`aria-label` or visible text).
   - Decorative mock controls should not create misleading functional behavior.
   - Keep responsive `flex` and `grid` layouts usable across expected window sizes.
   - Keep component markup readable and avoid broad refactors unrelated to the design change.

Only provide feedback for concrete issues introduced by the changed files. If a change is acceptable but could be a future improvement, do not block on it unless it conflicts with the design system above.
