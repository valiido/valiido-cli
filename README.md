# Valiido CLI

Access to the tenant ISMS for people and agents. The CLI uses the
Valiido HTTP API; all tenant, account and plan checks run on the server.

This is an initial MVP. It works with the matching Rails implementation in the
Valiido workspace. It has **not been deployed to production** and has no published
binary release yet.

## Build

Requires Go 1.24 or newer:

```sh
go build -o dist/valiido ./cmd/valiido
./dist/valiido --help
```

## Agent setup

```sh
./dist/valiido setup
# Or install for one agent only:
./dist/valiido setup --agent codex
./dist/valiido setup --agent claude
```

Installs the bundled [skill placeholder](skills/valiido/SKILL.md) into
`~/.agents/skills/valiido/SKILL.md` for Codex and
`~/.claude/skills/valiido/SKILL.md` for Claude Code. No login is needed.
Identical files are left unchanged; different existing skills are never overwritten.
Ensure `valiido` is on your PATH so the agent can run it. Restart the agent if
it does not discover the newly installed skill.

## Connect

```sh
./dist/valiido auth login --profile work
```

Open the displayed link, sign in and approve the displayed code. Check the
customer account, name the connection and confirm with your current password.
Only approve a code from a CLI session you just started. No password or long-lived
token is pasted into the terminal. Pairing requests expire after ten minutes.

Credentials are stored in the OS keyring. A headless machine can explicitly opt
into a private credential file:

```sh
./dist/valiido auth login --profile work --file-credentials
```

The file is mode `0600`, inside the user configuration directory `valiido/`
(normally `~/.config/valiido/` on Linux). There is no silent fallback to a file.
The profile locks while a command runs to prevent concurrent refresh-token use.
After a crash, remove that profile's `.lock` directory only after checking no CLI
process is still using it.

Connections are bound to one customer user and tenant. Current CLI pairings request
`isms:read isms:write` access. Existing connections keep their narrower scope.
Connections expire after 30 days. Access tokens last up to 15 minutes. Refresh
tokens rotate automatically. A disabled user, blocked tenant, policy restriction
or revoked connection cannot continue reading resources.

## Read documents

```sh
./dist/valiido auth status --profile work
./dist/valiido document list --profile work
./dist/valiido document read 42 --profile work
./dist/valiido document read 42 --text --profile work
./dist/valiido document read 43 --output policy.pdf --profile work
./dist/valiido api schema --profile work
./dist/valiido auth logout --profile work
```

`document list` automatically reads every page, including folders. It never
reports a partial listing as complete. IDs are tenant-scoped; the tenant comes
from the server-confirmed connection, not a command-line override.

`document read` returns plain text inside JSON for rich text documents. Folders
and PDFs return metadata. Use `--output` for a PDF download; the CLI does not
extract PDF text. Existing files are never overwritten. `--text` prints only a
rich text document's text, stripping terminal control characters.

## Read tasks and risks

```sh
./dist/valiido task list --profile work
./dist/valiido task read 42 --profile work
./dist/valiido risk list --profile work
./dist/valiido risk read 43 --profile work
./dist/valiido resource pdf risks 43 --output risk-43.pdf --profile work
```

Task and risk lists also retrieve every page and use tenant-scoped IDs. Detail
commands return the resource attributes and their localized display values as JSON.
`resource pdf TYPE ID` exports any resource available to the connection. Attached
document PDFs are downloaded unchanged; other resources use Valiido's PDF renderer.
Existing files are never overwritten.

## Read and change ISMS resources

```sh
./dist/valiido command list --profile work
./dist/valiido command run tasks.update \
  --input '{"id":42,"attributes":{"title":"Review access"}}' \
  --profile work
./dist/valiido command confirm CONFIRMATION_TOKEN --profile work
```

`command list` covers approved tenant ISMS resource areas. Connect, AuditMagic,
framework selection, support conversations, user access, permanent deletion, and
company, user, billing or account settings are excluded. It comes from
the same Rails command registry used by the web
application and includes the current input schema for every available action. Read
commands run immediately. Generic lists return at most 50 records and an opaque
tenant-bound `next_cursor`; continue until `complete` is true. Changes return a
short-lived confirmation token and do
not modify data until `commands confirm` is called. Show the proposed change to
the user before confirming it. The prepare/confirm sequence uses the same connected
profile and is an operational safety step rather than separate human authentication.
Use `command reject` to discard it. `--dry-run`
validates and previews a change without applying it.

JSON is the default (`--json` is accepted explicitly). Successful commands return
`{"ok":true,"data":...}`; errors return `{"ok":false,"error":{"code":...,"message":...}}`.
Pairing instructions go to stderr, tokens never to stdout. Logout revokes the
connection on the server before deleting local credentials. If the server is
unreachable, logout fails and retains credentials so revocation can be retried.

Exit codes: `0` success, `1` operational/protocol failure, `2` usage error,
`3` authentication failure, `4` forbidden, `5` not found.

## Local development

Start the Rails app after running its migrations. Then:

```sh
./dist/valiido auth login --server http://127.0.0.1:3000 --profile development
```

HTTP is permitted only for loopback IPs. Every other server must use HTTPS.
The server origin is stored with the credentials. Repository files cannot
override it and API redirects are refused.

- Browser documentation: `http://127.0.0.1:3000/api/` (browser login required).
- Machine-readable specification: `/api/openapi.json` (browser login or bearer token).
- Design research: [docs/fizzy-review.md](docs/fizzy-review.md).
- Agent usage: [docs/agents.md](docs/agents.md).

Rails generates OpenAPI and browser documentation from its routes and public
operation contract. CLI paths are generated by `bin/rails api:generate_cli` in
the Rails repository. There is no manually maintained OpenAPI snapshot. A separate SDK, documentation host and MCP server are
not needed for this MVP.

## Verify

```sh
bin/quality
```

Runs formatting checks, `go vet`, race-enabled tests and a build. Tests use local
HTTP fixtures and synthetic resources; they do not access customer accounts.

For the required cross-repository security and endpoint check, run
`bin/check-agent-api` in the sibling Rails repository. It checks generated routes,
builds this CLI, and exercises every public endpoint against the real Rails test
application, including pairing, reading, resource PDF exports, refresh and revocation.
No development or production connection is created. Set `CLI_DIR` if the CLI
checkout is not at `../valiido-cli`.

The short document, task, and risk commands are aliases for the same documented
Rails command endpoint used by every other ISMS resource. Reads and changes share
the command registry, authorization, tenant scoping, serialization, pagination,
and audit trail. Only PDF bytes use one generic resource download endpoint. Browser
UI routes remain unavailable to agent tokens. Changes use the shared Rails command
implementation with its authorization, audit and confirmation flow. There are no
separate JSON CRUD controllers for individual resources.
