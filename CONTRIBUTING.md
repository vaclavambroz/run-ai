# Contributing

Contributions are welcome. Please read this guide before opening a pull request.

## Getting started

1. Fork the repository and create a feature branch from `main`.
2. Run `make test` and `make lint` before submitting — the CI runs both.
3. Keep commits focused; one logical change per commit.
4. Write tests for new behaviour. Existing tests must pass.

## Code style

- Go: standard `gofmt` formatting, enforced by `make lint`.
- No external dependencies — the binary uses only the Go standard library.
- Comments explain *why*, not *what*. Well-named identifiers carry meaning.
- No half-implemented features. No code that cannot be reached.

## Pull requests

- Title: short imperative summary (under 70 characters).
- Description: explain what changes and why. Link related issues.
- One feature or fix per PR.

## Reporting issues

Use the GitHub issue tracker. For security vulnerabilities, see [SECURITY.md](SECURITY.md).

## License

By contributing you agree that your contributions will be licensed under the
project's [MIT License](LICENSE).
