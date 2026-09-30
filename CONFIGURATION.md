# Configuration storage

`config.json` is ordinary, readable JSON. Listener ports, routing rules, model
settings, upstream metadata, and other operational settings live there.
The default path is relative to the process's current working directory; use
`-config` with an absolute path when starting from another folder. Copying only
the executable or source code starts a new, empty installation.

Credentials live in `config.credentials.vault` beside it. This encrypted file
holds the admin username, password, and TOTP state; client and upstream API
keys; OAuth client credentials; and credentials embedded in URLs. Both files
are required for a working backup. The vault is protected for the operating
system account running aisense, using Windows DPAPI or the configured vault
key on other systems.

Starting aisense with an older encrypted or plaintext `config.json` migrates
it to the two-file layout. A damaged or missing credential store stops startup
instead of silently dropping keys.

To migrate an existing installation without starting its listeners, run
`go run ./cmd/migrateconfig -config config.json` while the old aisense process
is stopped.

To move an installation to another account or machine, run
`aisense -export-config=transfer.json` under the account that can open the
vault. The export contains **all credentials in plain text**. Protect it during
transfer, then start aisense with `-config transfer.json` at the destination;
it will create a new protected credential store there.
An encrypted vault copied from another Windows account, including a Windows
Sandbox account, cannot be opened there; use the complete export instead.
