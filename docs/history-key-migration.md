# Discord History Key Migration

This one-time utility converts the bridge's retained Megolm sessions into
room-scoped, passphrase-encrypted Element key export files. It does not rewrite
Matrix events, alter bridge state, or expose plaintext message content.

## Safety model

- The manifest is the authorization source of truth. A user receives keys only
  for the explicit room IDs listed under that user.
- Export is refused unless the user is currently joined to every selected room.
- Export is refused unless each room is an encrypted Discord portal whose
  current history visibility is `shared` or `world_readable`.
- Rooms that ever used `joined` or `invited` visibility are refused by default.
- Only sessions cryptographically tied to the bridge Olm account are exported.
- Every session ID, signing key, sender key, and encrypted-event reference is
  checked before a package is written.
- Partial sessions are refused by default.
- Key material and database credentials are never logged. Output files are
  created with mode `0600` under directories with mode `0700`.

The output is still highly sensitive. Anyone with both a package and its
passphrase can decrypt the selected room history.

## 1. Create and verify a backup

From the repository root:

```bash
./deploy.sh backup
```

This invokes the existing root backup script, waits for it to finish, and
checks that the matching encrypted PostgreSQL dump and signing-key archive
exist both locally and in Azure Blob. The current server backup intentionally
does not include the Synapse media store.

## 2. Build the exporter

```bash
./scripts/history-export.sh build
```

This copies only `tools/history-export` to the VM and builds
`matrix-history-export:local`. It does not restart or modify Matrix services.

## 3. Inventory retained sessions

```bash
./scripts/history-export.sh inventory > history-inventory.json
```

The report contains room IDs, names, and counts, but no cryptographic keys.

## 4. Prepare the authorization manifest

Start from `tools/history-export/manifest.example.yml`. Use room IDs, not room
names. Keep `shared_history: false` for ordinary user packages.

```yaml
version: 1
shared_history: false
users:
  "@cobular:nasa.matrix.cobular.com":
    rooms:
      - "!DVqwqIFivvBDPIXpVT:nasa.matrix.cobular.com"
```

Set `shared_history: true` only for a deliberately clean history-steward
account. That allows the imported keys to be redistributed by future MSC4268
invites. It is not needed for a recipient to decrypt their own package.

## 5. Validate before exporting

Invite each user to the listed rooms and have them join, then run:

```bash
./scripts/history-export.sh validate migration.yml > migration-validation.json
```

Validation unpickles and checks every selected session but writes no package.

## 6. Generate encrypted packages

```bash
./scripts/history-export.sh export migration.yml
```

The command prints the remote output directory. It contains:

```text
packages/       # encrypted Element key export files
passphrases/    # one random passphrase per user
report.json     # counts and SHA-256 hashes, no key material
```

Deliver the package and passphrase through separate channels where practical.
In Element, the recipient imports the package from the room-key import UI.
Test old events in at least one large and one small room before deleting the
transfer copies.

For a user who gains another room later, create a new manifest containing only
that user and room, validate it, and generate a delta package.

## Partial sessions

A session whose first retained Megolm index is greater than zero cannot decrypt
the beginning of that session. The utility fails rather than silently producing
incomplete history. After reviewing the validation failure, an operator can run
the container manually with `--allow-partial-sessions` to acknowledge that
limitation. The wrapper intentionally does not enable this override.

## Cleanup

After recipients validate their imports:

1. Confirm their Element key backup/recovery is configured.
2. Delete remote transfer packages and passphrases.
3. Delete local transfer copies.
4. Keep only the non-secret report if an audit record is useful.

Do not delete or alter the bridge's crypto database as part of this migration.