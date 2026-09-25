# Valiido CLI

`valiido` connects your terminal or coding agent to a Valiido ISMS account. Build it with Go 1.24 or newer.

```sh
go build -o dist/valiido ./cmd/valiido
./dist/valiido --help
```

## Quick start

```sh
valiido auth login --profile work
valiido documents list --limit 20 --profile work
valiido documents show 42 --profile work
valiido open documents 42 --profile work
```

Approve the pairing code in your browser. Credentials use the OS keyring by default. Use `valiido auth logout --profile work` to revoke the connection.

The CLI checks the `Valiido-Tenant` and `Valiido-Scope` response headers against the selected profile before reading the body or returning a confirmation token. For older servers that omit both headers, it checks `GET /api/v1/me` once per invocation, before using the first response.

Use `valiido more` for ISMS areas, leaf `--help` for flags, and `valiido command list` for live actions and input schemas. `valiido command run NAME` runs a catalog action. `valiido api schema` prints the connected API contract.

## Changes and imports

Read the latest resource version before editing. Preview with the same input and profile you will use to prepare the change.

```sh
valiido tasks update --input-file patch.json --dry-run --profile work
valiido tasks update --input-file patch.json --json --profile work |
  valiido command confirm - --profile work
```

Use the preparation output with `valiido command reject -` to discard it. If a confirmation loses its response, read the affected record before preparing again. On a version conflict, read and review the newer state.

```sh
valiido resource import assets --file assets.json --dry-run --profile work
valiido resource import assets --file assets.json --profile work --json
```

Imports take a JSON array and validate the whole file. Confirm each returned batch separately. See the [import guide](skills/valiido/references/importing.md) for source conversion and verification.

## Output and recovery

| Need | Option |
| --- | --- |
| JSON envelope with data, summary, breadcrumbs and retry count | `--json` or pipe stdout |
| JSON data only | `--quiet` |
| Filter the envelope using local jq | `--jq EXPR` |
| A compact result | `--ids-only`, `--count`, `--markdown`, or `--styled` |

Use the error code, hint and recovery breadcrumb to diagnose a failure. `valiido doctor --profile work` checks the connection. See `valiido help output`, `valiido help exit-codes` and `valiido help environment` for details.

`--limit N` caps returned rows. Follow the `load_more` breadcrumb or reuse the cursor with the same query and profile. The CLI retries transient reads and command requests. Upload initialization and token exchange only retry rate-limit responses. Confirmations replay the same token.

## Agent setup and development

```sh
valiido setup
valiido shell-completion bash
```

`valiido setup --agent codex` and `valiido setup --agent claude` install the [agent skill](skills/valiido/SKILL.md) for one agent. Put the binary on the agent's PATH.

HTTP logins require a loopback address. Use `./dist/valiido auth login --server http://127.0.0.1:3000 --profile development` against a local Rails server.

Run `bin/quality` for format, vet, race tests and builds. Run `bin/check-agent-api` from the Rails checkout to verify both repos against the public API contract.
