# Valiido CLI

Use Valiido from a terminal or coding agent. The CLI connects to one Valiido
account and discovers the available ISMS resources and actions from the live API.

This repository contains the initial MVP. There is no published binary release
yet.

## Build

Requires Go 1.24 or newer:

```sh
go build -o dist/valiido ./cmd/valiido
./dist/valiido --help
```

## Get started

```sh
valiido auth login --profile work
valiido command list --profile work
valiido resource list documents --profile work
valiido resource read documents 42 --profile work
valiido resource examples documents --query scope --profile work
```

The login command prints a browser link and pairing code. Open the link, sign in
to Valiido and approve the connection. Credentials are stored in the operating
system keyring. Run `valiido auth logout --profile work` to revoke the connection.

Changes use the commands advertised by the connected account:

```sh
valiido command run tasks.update \
  --input '{"id":42,"attributes":{"title":"Review access"}}' \
  --profile work
valiido command confirm CONFIRMATION_TOKEN --profile work
```

A write command prepares a change first. It is applied only after
`command confirm`; use `command reject` to discard it.

## Documentation

- Public API reference: <https://app.valiido.com/api>
- Machine-readable specification: <https://app.valiido.com/api/openapi.json>
- CLI reference: `valiido --help`
- Live resources, actions and input schemas: `valiido command list`
- Connected API schema: `valiido api schema`

The API reference is generated from the Rails routes, the public operation
contract and the same command registry used to execute requests. Resource and
command lists are therefore not copied into this README.

## Agent setup

```sh
valiido setup
# Or install for one agent only:
valiido setup --agent codex
valiido setup --agent claude
```

This installs the bundled [Valiido skill](skills/valiido/SKILL.md) for Codex,
Claude Code, or both. Ensure `valiido` is on the agent's `PATH` and restart the
agent after installing the skill.

## Local development

With the Rails application running at `http://127.0.0.1:3000`:

```sh
go build -o dist/valiido ./cmd/valiido
./dist/valiido auth login \
  --server http://127.0.0.1:3000 \
  --profile development
```

HTTP is accepted only for loopback development servers. Other server origins
must use HTTPS.

## Verify

```sh
bin/quality
```

The Rails repository provides `bin/check-agent-api` for the cross-repository API
contract and security checks.
