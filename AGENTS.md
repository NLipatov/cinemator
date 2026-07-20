# Git

- Create a commit only after explicit confirmation for that exact commit.
- Use Conventional Commits for every commit and pull request title:
  `type(scope): description`.
- Use `feat` for user-visible features and `fix` for user-visible bug fixes.
- Use descriptive lowercase branch names with a change-type prefix, such as
  `feat/`, `fix/`, `docs/`, `refactor/`, `test/`, `ci/`, or `chore/`.

# Code

- Treat code as a liability and prefer the smallest complete solution.
- Reuse or delete code before adding new code or abstractions.
- Optimize for correctness and clarity, not output volume.

# Tests

- Keep production code easy to test through clear responsibilities and
  boundaries.
- Do not add production APIs, branches, hooks, or abstractions solely for tests.
- Test through public behavior whenever possible.
- Keep test-only helpers package-private.

# Streaming performance

- Streaming changes must not regress the startup and playback-continuity SLOs in
  `docs/playback-qoe.md`; run the required QoE tests when changing the playback
  critical path.
