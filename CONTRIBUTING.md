# Contributing

1. Open an issue first for anything larger than a small fix.
2. Build and test: `make check`. See [docs/development.md](docs/development.md).
3. Keep changes small and say in the pull request what you tested and on which
   distribution.

UI rules (the full set is in [DESIGN.md](DESIGN.md)):

- No animation, low resource use.
- Terse text: state names and values; explanations only in "?" popovers.
- Text is never truncated with an ellipsis; layouts must fit.
- Status is a coloured dot and the state name (Running, Offline, Encrypted). No coloured text, no toggles.
- Use the themed `Select`, `ComboInput` and `Menu` components, not native ones.

Contributions are accepted under the [Apache License 2.0](LICENSE), the
project's licence.
