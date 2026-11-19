// From repostd's node standard (ADR 0002 there), copied rather than synced:
// this is a Go repo, whose TypeScript is harness/. Repo additions go only
// inside the repostd:local block.
import { defineConfig } from 'oxfmt'

// Remix 3's own formatting (remix-run/remix's oxfmt.config.ts).
export default defineConfig({
  printWidth: 100,
  semi: false,
  singleQuote: true,
  useTabs: false,
  sortPackageJson: false,
  ignorePatterns: [
    // Written, and formatted, by the tools that own them: pnpm's lockfile,
    // repocheck's managed skill, and the ADR index repocheck generates.
    'pnpm-lock.yaml',
    '.agents/skills/repostd/',
    'docs/adr/README.md',
    // Committed generated files, so that `oxfmt --check` does not fight their
    // generator.
    // repostd:local ignores
    'harness/runtime/settings.mjs',
    // repostd:end
  ],
})
