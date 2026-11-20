# Valiido CLI

Your Valiido ISMS in the terminal: documents, tasks, risks, assets, audits and more.
Built for people and for coding agents such as Claude Code and Codex.

[Valiido](https://valiido.com) · [Releases](https://github.com/valiido/valiido-cli/releases) · [Agent skill](skills/valiido/SKILL.md)

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/valiido/valiido-cli/main/install.sh | sh
```

Linux and macOS, amd64 and arm64. Windows: download the zip from [Releases](https://github.com/valiido/valiido-cli/releases).
Update with `valiido upgrade`; the CLI also tells you when a new release is out.

## Set up

```sh
valiido auth login   # approve the pairing code in your browser
valiido doctor       # check login and connection
```

Credentials stay in the OS keyring. Use `--profile NAME` for more than one account.

## Agent

```sh
valiido setup --agent claude   # or codex, or all
```

This installs the Valiido skill, so the agent knows how to read records, prepare
changes and run imports. Then just ask, for example "Which risks have no owner?".

- `valiido command list` lists every action with its JSON input schema.
- `valiido api schema` prints the API description (OpenAPI).
- Output is JSON when piped; errors carry a stable code, a hint and the next command.
- Changes are prepared first and only apply after `valiido command confirm`.

## Development

`bin/quality` runs format, vet, tests and builds. Pushing a `v*` tag builds and
publishes the release archives (`bin/release`).
