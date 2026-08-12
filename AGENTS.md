# Repository Workflow

These instructions apply to all AI agents and contributors working in this
repository. Follow them unless the repository owner explicitly requests a
different workflow for a specific task.

## Remotes

- `upstream` is the original repository: `https://github.com/Wei-Shaw/sub2api.git`.
- `origin` is the customized fork: `git@github.com:zuchengchen/sub2api.git`.
- Never push to `upstream`.

## Long-Lived Branches

### `main`

- `main` is a clean mirror of `upstream/main`.
- Never add custom commits, fixes, configuration, or workflow files to `main`.
- Update it only with a fast-forward from `upstream/main`:

  ```bash
  git fetch upstream --prune
  git switch main
  git merge --ff-only upstream/main
  git push origin main
  ```

- Never force-push `main`.
- If `main` and `upstream/main` have diverged, stop and report the divergence;
  do not merge, rebase, reset, or force-push without the owner's approval.

### `main-czc`

- `main-czc` is the stable customized and production-ready branch.
- It contains tested local changes plus selected, tested updates from `main`.
- It is not expected to have the same commit ID as `upstream/main`.
- Do not develop features directly on `main-czc`.
- Merge into it only after relevant tests pass.
- Preserve merge ancestry for releases, upstream syncs, and hotfixes. Do not
  squash upstream synchronization commits.
- Never force-push `main-czc`.
- Deploy immutable `czc-*` tags when practical instead of an untagged moving
  branch. Suggested tag format: `czc-vYYYY.MM.DD.N`.

### `dev-czc`

- `dev-czc` is the integration branch for customized development.
- Start normal work from an up-to-date `dev-czc`.
- Keep it buildable and testable; incomplete work should remain on short-lived
  branches or behind a feature flag.
- Never force-push `dev-czc`.

## Short-Lived Branches

- Create feature and fix branches from `dev-czc`:
  - `feature/<short-name>` for new functionality.
  - `fix/<short-name>` for non-production fixes.
- Merge tested feature/fix branches into `dev-czc`, preferably through a pull
  request. Squash merging is acceptable for self-contained feature work.
- Delete short-lived branches after they are merged.

## Releasing Customized Changes

1. Develop and test changes on `feature/*` or `fix/*` branches.
2. Merge them into `dev-czc` and run the appropriate integration tests.
3. Merge the tested `dev-czc` state into `main-czc` through a pull request or an
   explicit merge commit.
4. Run release checks on `main-czc` and create a `czc-*` tag for production
   releases.
5. Never promote changes by rewriting branch history.

## Synchronizing Upstream Changes

Upstream changes must be tested with the customized code before reaching
`main-czc`.

1. Fast-forward `main` from `upstream/main` and push `main` to `origin`.
2. Start a temporary branch from the current stable branch:

   ```bash
   git switch main-czc
   git switch -c sync/upstream-YYYYMMDD
   git merge main
   ```

3. Resolve conflicts on the `sync/*` branch without discarding local behavior.
4. Run relevant backend, frontend, migration, and integration tests according
   to the affected files.
5. Merge the tested `sync/*` branch into `main-czc` with its ancestry intact.
   Do not squash this merge.
6. Merge the updated `main-czc` back into `dev-czc` so future development uses
   the same upstream baseline.
7. Delete the temporary `sync/*` branch after successful integration.

Do not merge `main` independently into both `main-czc` and `dev-czc`; that
creates two conflict-resolution paths. Integrate upstream into `main-czc` once,
then propagate `main-czc` to `dev-czc`.

## Hotfixes

1. Create `hotfix/<short-name>` from `main-czc`.
2. Make the smallest safe fix and run focused regression tests.
3. Merge the hotfix into `main-czc` without squashing its release history.
4. Merge the updated `main-czc` back into `dev-czc` immediately.
5. Tag the repaired release when it is deployed.

## Required Agent Checks

Before editing files, an AI agent must:

1. Read this file completely.
2. Run `git status --short --branch` and identify the current branch.
3. Preserve all existing user changes and never discard unrelated work.
4. For normal feature work, ensure the work is on `dev-czc` or a branch created
   from it. If currently on `main`, do not edit; switch to the appropriate custom
   branch first.
5. Fetch remotes before any synchronization or release operation and inspect
   branch divergence before merging.

Before finishing, the agent must report:

- The branch on which changes were made.
- The commits or merges created, if any.
- Tests and checks that were run, including any that could not be run.
- Whether the work still needs promotion from a feature branch to `dev-czc` or
  from `dev-czc` to `main-czc`.

## Safety Rules

- Do not use force-push, destructive reset, history rewriting, or branch
  deletion unless the repository owner explicitly authorizes the exact action.
- Do not commit secrets, credentials, environment files, generated artifacts,
  or unrelated formatting changes.
- Do not push or merge merely because implementation is complete. Pushing,
  opening a pull request, merging, tagging, and deploying require an explicit
  user request unless they are already part of the active task.
- Match the scope of testing to the affected code. Upstream syncs and releases
  require broader verification than isolated feature changes.
