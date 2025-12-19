# Contributing to Odac

First off, thank you for considering contributing to Odac! It's people like you that make Odac such a great tool.

This project and everyone participating in it is governed by the [Odac Code of Conduct](CODE_OF_CONDUCT.md). By participating, you are expected to uphold this code. Please report unacceptable behavior to security@odac.run.

## How Can I Contribute?

There are many ways to contribute, from writing tutorials or blog posts, improving the documentation, submitting bug reports and feature requests or writing code which can be incorporated into Odac itself.

### Reporting Bugs

Before creating a bug report, please check the [issues list](https://github.com/Odac/Odac/issues) to see if the bug has already been reported. If it has, please add a comment to the existing issue instead of creating a new one.

When you are creating a bug report, please include as many details as possible. Fill out the required template, the information it asks for helps us resolve issues faster.

### Suggesting Enhancements

If you have an idea for a new feature or an improvement to an existing one, please open an issue with the "feature request" label. Provide a clear and detailed explanation of the feature, why it's needed, and how it should work.

### Your First Code Contribution

Unsure where to begin contributing to Odac? You can start by looking through `good first issue` and `help wanted` issues:

*   [Good first issues](https://github.com/Odac/Odac/issues?q=is%3Aissue+is%3Aopen+label%3A%22good+first+issue%22) - issues which should only require a few lines of code, and a test or two.
*   [Help wanted issues](https://github.com/Odac/Odac/issues?q=is%3Aissue+is%3Aopen+label%3A%22help+wanted%22) - issues which should be a bit more involved than `good first issue` issues.

### Development Workflow

Our development process follows a branching model similar to Git Flow. This helps us keep the `main` branch stable and ready for release at all times.

1.  **Branching:**
    *   All new features or bug fixes should be developed on a feature branch.
    *   Create your feature branch from the `dev` branch: `git checkout -b your-feature-name dev`.
    *   Never create a feature branch directly from `main`.

2.  **Making Changes:**
    *   Make your code changes, ensuring you follow the project's coding standards.
    *   Add tests for any new code.
    *   Ensure all tests pass (`npm test`) and the code lints correctly.

3.  **Committing Changes:**
    *   This project uses **Conventional Commits**. This is a requirement for our automated release process.
    *   Your commit messages must be in the format: `<type>(<scope>): <subject>`.
        *   **type:** `feat` (new feature), `fix` (bug fix), `docs`, `style`, `refactor`, `test`, `chore`.
        *   **scope:** (Optional) The part of the project you're working on (e.g., `server`, `framework`).
    *   Example: `feat(server): add rate limiting to API endpoints`

4.  **Submitting a Pull Request:**
    *   Once your feature is complete, push your branch to your fork.
    *   Open a Pull Request (PR) from your feature branch to the `dev` branch of the main repository.
    *   Provide a clear description of the changes in your PR.
    *   After your PR is reviewed and merged, you can safely delete your feature branch.

The `main` branch is only updated by merging `dev` into it during a new release. Direct pushes or PRs to `main` are not permitted.

## Code Style

All JavaScript must adhere to the style defined in the `.prettierrc` file. The linter will automatically check this.

---

We look forward to your contributions!
