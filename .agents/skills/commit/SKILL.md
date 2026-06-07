---
name: commit
description: Analyze outstanding git changes and organize them into logical, well-structured commits. Use when the user wants to commit changes or organize their work into commits.
allowed-tools: Bash(git status, git diff, git add, git commit, git log, git reset), Read, Glob, Grep, AskUserQuestion
---

# Smart Commit

Analyze outstanding changes and organize them into logical, well-structured commits.

## Process

1. **Check current state**:
   - Run `git status` to see all modified, staged, and untracked files
   - Run `git diff` to see unstaged changes
   - Run `git diff --cached` to see already staged changes

2. **Analyze changes**:
   - Read the changed files to understand what was modified
   - Group related changes by:
     - Feature/functionality (e.g., all auth-related changes together)
     - Type (e.g., refactoring, bug fix, new feature, docs, tests)
     - Component/module (e.g., all UI changes, all API changes)

3. **Propose commit groups**:
   - Present the proposed groupings to the user via AskUserQuestion
   - Each group should have:
     - List of files to include
     - Proposed commit message following conventional commits format
   - Ask user to confirm, adjust, or skip each group

4. **Create commits**:
   - For each approved group:
     - Stage only the relevant files with `git add <files>`
     - Create commit with the agreed message
   - Use `git add -p` approach if only parts of a file belong to a commit (ask user first)

5. **Summary**: Show final `git log --oneline -n <count>` of created commits

## Commit Message Format

Follow conventional commits:
```
<type>(<scope>): <description>

[optional body]

Co-Authored-By: Claude <noreply@anthropic.com>
```

Types: `feat`, `fix`, `refactor`, `docs`, `test`, `chore`, `style`, `perf`

## Examples

**Single logical change:**
```
feat(auth): add OAuth2 login flow

- Add OAuth2 provider configuration
- Implement token refresh logic
- Add login/logout UI components
```

**Mixed changes get split:**
- `fix(api): handle null response in user endpoint`
- `refactor(ui): extract Button component`
- `docs: update README with setup instructions`

## Safety

- Never commit files that look like secrets (.env, credentials, keys)
- Always show the user what will be committed before committing
- If unsure about grouping, ask the user
