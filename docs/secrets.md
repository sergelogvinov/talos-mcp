# Talos MCP Server — Encrypted Secrets in talosconfig

Status: **implemented** (design §15.2 step 13).

## 1. Problem

A talosconfig context holds two secrets that talos-mcp uses:

| Field            | What it is                                  | Impact if leaked |
| ---------------- | ------------------------------------------- | ---------------- |
| `key`            | private key of the Talos client certificate | full use of the certificate's role (`os:reader` / `os:operator`) on the cluster |
| `cluster_secret` | discovery service key (design §2.2)         | read the node inventory; **forge affiliates**, which KubeSpan nodes trust as WireGuard peers |

Both are stored in plain base64 today. Anyone who can read the file, a backup
of it, or a copy in git or a chat attachment can use them. SSH solved the same
problem for private keys long ago: the key file is encrypted with a
passphrase, and the passphrase is supplied when the key is used.

This document adds the same option to talos-mcp. Each secret field may hold
an **encrypted** value, which the server decrypts in memory at startup.

## 2. Goals and Non-goals

Goals:

- Encrypt `key` and `cluster_secret` at rest, field by field. Non-secret
  fields (`endpoints`, `ca`, `crt`, `cluster_id`) stay readable, so the role
  can still be read from `crt` (design §2.3) and the file stays easy to
  review.
- Work like SSH: unlock with a passphrase, or without one by using a key
  file. That key file may be an existing SSH key.
- Never prompt on stdin in `mcp` mode, because stdin is the MCP transport.
- Plaintext and encrypted fields can be mixed, so existing talosconfigs keep
  working.
- Include CLI helpers to encrypt an existing talosconfig.

Non-goals:

- Protecting secrets **at runtime**. Once decrypted, they live in the
  server's memory, and a compromised server process can read them. This
  feature protects data at rest only.
- Encrypting `ca` or `crt`. They are public.
- Making the encrypted file usable by `talosctl`. It isn't (§8).
- KMS or Vault integration. That is future work (§10).

## 3. Format

Encrypted values use **[age](https://age-encryption.org)** in ASCII armor,
base64-encoded like every other talosconfig value:

```yaml
contexts:
  prod-eu:
    endpoints: [10.0.0.10]
    ca: LS0tLS1CRUdJTi...          # plain (public)
    crt: LS0tLS1CRUdJTi...         # plain (public), role is read from here
    key: LS0tLS1CRUdJTiBBR0UgRU5DUllQVEVEIEZJTEUtLS0tLQ...   # age-encrypted
    discovery:
      cluster_id: 3x9y...
      cluster_secret: LS0tLS1CRUdJTiBBR0UgRU5DUllQVEVEIEZJTEUtLS0tLQ...  # age-encrypted
```

Detection works per field. The server base64-decodes the value, and if the
result begins with `-----BEGIN AGE ENCRYPTED FILE-----`, the field is
encrypted. Otherwise it is treated as plaintext, which is today's behavior.

Why age:

- **One envelope for both fields.** `key` is a PEM private key, but
  `cluster_secret` is just 32 random bytes. Key-specific formats such as
  encrypted PKCS#8 can't wrap the secret.
- **SSH-like unlocking.** age supports the three modes we need: a passphrase
  (scrypt), a native age identity file, and **SSH keys** as recipients
  (`ssh-ed25519` and `ssh-rsa`, through `filippo.io/age/agessh`). A field can
  be encrypted to several recipients at once, for example a team's SSH public
  keys plus the server's own age identity.
- It is a small, audited Go library (`filippo.io/age`) with no cgo and no
  configuration knobs. It also works with plugins such as
  `age-plugin-yubikey` for hardware tokens (see §10).

A field encrypted with a passphrase uses age's scrypt recipient. age
requires a scrypt stanza to be the only recipient, so a value is encrypted
either with a passphrase or to a set of recipients, never both.

## 4. Unlock Sources

Decryption needs either a **passphrase** or one or more **identities**. Both
kinds of source can be configured together. For each field the server tries
the identities first, then the passphrase.

| Source                 | Flag / Env                                                       | Notes |
| ---------------------- | ---------------------------------------------------------------- | ----- |
| Identity file(s)       | `--talosconfig-identity` / `TALOSCONFIG_IDENTITY` (repeatable, comma-separated) | age identity file (`AGE-SECRET-KEY-1...`) or an OpenSSH private key (`~/.ssh/id_ed25519`). Best for servers: no secret is typed. |
| Passphrase file        | `--talosconfig-passphrase-file` / `TALOSCONFIG_PASSPHRASE_FILE`  | First line of the file, without the trailing newline. Recommended for Kubernetes: mount it from a Secret. |
| Askpass program        | `--talosconfig-askpass` / `TALOSCONFIG_ASKPASS`                  | The program gets the prompt as its argument and prints the passphrase on stdout, like `SSH_ASKPASS`. Works with a GUI prompt, `pass`, 1Password CLI, or macOS `security find-generic-password -w`. It runs with no stdin and a 60s timeout. |
| Passphrase environment variable | `TALOSCONFIG_PASSPHRASE` (env only, no flag)            | For MCPB bundles (§7). Discouraged elsewhere: environment variables can leak through `/proc/<pid>/environ`, crash dumps and child processes. The server clears it from its own environment after reading it. |
| Terminal prompt        | automatic                                                        | **Only** for `tools` and `config`, and only when `/dev/tty` is available and no passphrase source above is set. Identity files may be set: the prompt then asks for an encrypted SSH key's passphrase. It is **never** used in `mcp` or `server` mode. |

When several passphrase sources are set, the first in the table wins: file,
askpass, environment variable, prompt.

Encrypted SSH private keys used as identities are supported. Their
passphrase comes from the same passphrase sources (file, askpass, env), and
the server asks for it only when it needs it. `ssh-agent` isn't supported,
because age can't decrypt through the agent protocol (§10).

Rejected source: **MCP elicitation.** The MCP spec doesn't allow servers to
request sensitive information through elicitation, and the passphrase could
pass through the client or the model's context.

## 5. Lifecycle

1. **Load.** `clientconfig.FromBytes` parses the talosconfig. Encrypted `key`
   values are just opaque base64 to it. The discovery block is read as
   described in design §2.2.
2. **Classify.** For each context, the server reads the role from `crt`
   (design §2.3) and marks each secret field as plain or encrypted. This
   needs no unlock source.
3. **Unlock.** If any field is encrypted, the server gets the unlock
   material once (§4) and **decrypts every encrypted field at startup**, so
   a wrong passphrase fails fast rather than on the first tool call.
4. **Replace in memory.** The decrypted `key` is base64-encoded again and
   written into the in-memory `clientconfig.Context` before
   `client.New(...)`, so the Talos client library sees a normal config. The
   decrypted `cluster_secret` goes directly into the discovery reader.
5. **Discard.** The passphrase and the age identities are dropped after
   startup: the server zeroes the byte slices it controls and keeps no
   references. Only the decrypted secrets stay in memory, as long as they
   would without encryption.

Error handling:

- A context whose fields can't be decrypted (no matching identity, or a wrong
  passphrase) is **skipped**, with an error naming the context and field.
  The error never includes the ciphertext or any partial plaintext.
- If every context is skipped, startup fails. In `server` mode,
  `--require-all-contexts` (default `false`) makes any skipped context fatal.
- If encrypted fields exist but no unlock source is configured, startup fails
  with a message listing the available sources, for example "talosconfig has
  encrypted fields; set --talosconfig-identity or
  --talosconfig-passphrase-file".
- A plaintext `key` in `server` mode logs one info-level hint recommending
  encryption. It isn't a warning, because plaintext is still valid.

Each value is decrypted once per process start. The file isn't watched, so
changes need a restart, which matches design §13 (no hot reload).

## 6. CLI Helpers

The new `config` subcommand handles encryption without needing a separate
`age` binary:

```sh
# Encrypt key and cluster_secret of every context with a passphrase (prompts twice on /dev/tty)
talos-mcp config encrypt --talosconfig ~/.talos/config -o ~/.talos/mcp-config --passphrase

# Encrypt to SSH and age public keys (no passphrase needed to unlock)
talos-mcp config encrypt -o mcp-config \
    --recipient-file ~/.ssh/id_ed25519.pub \
    --recipient age1qyqszqgpqyqszqgpqyqszqgpqyqszqgpqyqszqgpqyqszqgpqysq8lh2qp

# Only some contexts or fields
talos-mcp config encrypt -o mcp-config --context prod-eu --field key --passphrase

# Check that everything decrypts (prints context → role, which fields are encrypted, ok/error)
talos-mcp config check --talosconfig mcp-config --talosconfig-identity ~/.ssh/id_ed25519

# Decrypt to stdout, e.g. to restore a talosctl-compatible file
talos-mcp config decrypt --talosconfig mcp-config --talosconfig-identity ~/.ssh/id_ed25519
```

Rules:

- `encrypt` never overwrites the input file. `-o` is required, and the output
  is created with mode `0600`.
- Fields that are already encrypted are left as they are, so running the
  command again is safe. To re-encrypt (rotate), use `decrypt` then
  `encrypt`.
- `decrypt` writes to stdout only. Writing it to a file is the user's choice.
- Encrypted output is plain YAML with base64 values, so it can be diffed and
  committed. Committing it is only safe when the encryption uses recipients
  (or a strong passphrase) and the identities aren't in the same repository.

## 7. Deployment Recipes

**Local, Claude Desktop/Code (stdio):**

- Encrypt to your own SSH key and set
  `TALOSCONFIG_IDENTITY=~/.ssh/id_ed25519`. If that SSH key has a passphrase,
  add `TALOSCONFIG_ASKPASS` pointing to your keychain helper.
- MCPB bundle: `manifest.json` gets a `user_config` entry,
  `talosconfig_passphrase`, with `"sensitive": true`. The host stores it in
  the OS keychain and passes it as `TALOSCONFIG_PASSPHRASE`. This is the one
  case where the environment variable is the right choice.

**Kubernetes (server mode, Helm):**

- Encrypt the talosconfig to an age recipient that belongs to this
  deployment. The encrypted talosconfig can then live in a ConfigMap or in
  git.
- The age identity is stored in a **separate** Secret, mounted as a file, and
  passed with `--talosconfig-identity`. Chart values:
  `talosconfig.encryption.identitySecret` or
  `talosconfig.encryption.passphraseSecret`.
- Reading the talosconfig alone is no longer enough to get credentials. An
  attacker also needs the identity Secret, which RBAC can restrict on its
  own.

## 8. Compatibility

- **talosctl can't use encrypted contexts.** It would fail to parse `key`
  ("failed to find any PEM data"). Keep the encrypted talosconfig as an
  MCP-only file. Design §2.2 already recommends this for the `discovery`
  block.
- `talosctl config` commands rewrite the file. `key` is a known field, so
  its encrypted value survives the rewrite. `cluster_secret` is in the
  unknown `discovery` block and is still dropped (design §2.2).
- Plaintext talosconfigs work unchanged, and no flags are required for them.

## 9. Package Layout & Dependencies

```
internal/secrets/
    detect.go     IsEncrypted(base64Value) bool
    unlock.go     Unlocker: identities + passphrase sources (§4), lazy SSH-key passphrase
    decrypt.go    Decrypt(value, Unlocker) ([]byte, error)
    encrypt.go    Encrypt(plain, recipients | passphrase) (base64 armored age)
internal/config/  calls secrets during talosconfig load (§5 steps 2–4)
cmd/talos-mcp/
    config.go     `config encrypt|decrypt|check` subcommands
```

Layering: only `internal/config` (when loading) and `cmd/talos-mcp/config.go`
use `internal/secrets`. `internal/talos` and `internal/tools` only ever see
decrypted, in-memory configuration.

New dependencies:

- `filippo.io/age` and `filippo.io/age/armor`
- `filippo.io/age/agessh`, for SSH keys as recipients and identities
- `golang.org/x/term`, for the passphrase prompt on `/dev/tty` (`tools` and
  `config` only)

## 10. Alternatives & Future Work

| Option | Status |
| ------ | ------ |
| Encrypted PKCS#8 PEM (`-----BEGIN ENCRYPTED PRIVATE KEY-----`, PBES2) for `key` | Not chosen. It only covers `key`, not `cluster_secret`. The Go standard library can't decrypt it, so a third-party library would be needed. It also has no SSH-key unlock. It could be added later as a second accepted format for `key` if users expect `openssl pkcs8 -topk8 -v2 aes256` output to work. |
| Legacy PEM encryption (`Proc-Type: 4,ENCRYPTED`) | Rejected. It is insecure (weak key derivation), and Go has deprecated its support for it. |
| SOPS-encrypted talosconfig | Works today without changes: decrypt with `sops exec-file` before starting. It encrypts the whole file, not single fields. It isn't built in. |
| ssh-agent unlock | Not possible with age. Agent signing can't do the X25519/RSA-OAEP key unwrap that age needs. |
| Hardware tokens (YubiKey, Secure Enclave) | Future work, through age plugins (`age-plugin-yubikey`, `age-plugin-se`). The plugins need a TTY or GUI for touch or PIN, so they suit local stdio use rather than servers. |
| KMS / Vault | Future work. `kms://` or `vault://` value prefixes, resolved at startup, would let a server deployment avoid holding any long-lived unlock material. |

## 11. Testing

- `internal/secrets`:
  - Round-trip tests for passphrase, age X25519, `ssh-ed25519`, `ssh-rsa`
    and multiple-recipient encryption.
  - A wrong passphrase or identity returns an error with no plaintext.
  - Detection must not misclassify plaintext PEM or random base64.
  - An encrypted SSH identity asks for its passphrase lazily.
- `internal/config`:
  - A mixed talosconfig with plaintext and encrypted contexts.
  - A context is skipped when its fields can't be decrypted.
  - Startup fails when encrypted fields exist but no unlock source is set.
  - The role is read from `crt` without unlocking anything.
- `cmd/talos-mcp`:
  - `config encrypt`, `check` and `decrypt` round-trip.
  - Fields that are already encrypted are left as they are.
  - The output file has mode `0600`.
  - `mcp` mode never opens `/dev/tty`.
  - `TALOSCONFIG_PASSPHRASE` is removed from the environment after it is
    read.
