# Configuring talosconfig for talos-mcp

talos-mcp connects to Talos clusters with a **talosconfig**, the same file
format `talosctl` uses. This guide covers every field the server reads, the
command-line flags and environment variables that control loading, and how to
import a context from a machine config, and encrypt and decrypt the secret
fields.

## 1. Quick start

```sh
# Create a read-only client credential for the MCP server
talosctl config new --roles os:reader --crt-ttl 8760h ~/.talos/mcp-config

# Or build the whole context, discovery block included, from the control
# plane machine config, encrypted to your SSH key (see 4.3)
talos-mcp config import controlplane.yaml -o ~/.talos/mcp-config \
    --recipient-file ~/.ssh/id_ed25519.pub

# Optional: encrypt the private key to your own SSH key
talos-mcp config encrypt --talosconfig ~/.talos/mcp-config -o ~/.talos/mcp-config.enc \
    --recipient-file ~/.ssh/id_ed25519.pub

# Check that the file loads and decrypts
talos-mcp config check --talosconfig ~/.talos/mcp-config.enc \
    --talosconfig-identity ~/.ssh/id_ed25519

# Run the MCP server over stdio
talos-mcp mcp --talosconfig ~/.talos/mcp-config.enc \
    --talosconfig-identity ~/.ssh/id_ed25519
```

## 2. File format

```yaml
context: prod-eu                  # default context (cluster)
contexts:
  prod-eu:
    endpoints: [10.0.0.10, 10.0.0.11, 10.0.0.12]
    nodes: [10.0.0.10]            # optional
    ca: LS0tLS1CRUdJTi...         # base64 PEM, CA certificate
    crt: LS0tLS1CRUdJTi...        # base64 PEM, client certificate
    key: LS0tLS1CRUdJTi...        # base64 PEM, client private key (plain or encrypted)
    discovery:                    # optional, enables talos_clusters_members
      endpoint: discovery.talos.dev:443
      cluster_id: 3x9y...
      cluster_secret: c2VjcmV0... # base64, 32 bytes (plain or encrypted)
  staging:
    endpoints: [staging-api.example.com]
    ca: LS0t...
    crt: LS0t...
    key: LS0t...
```

**One context is one cluster.** Every tool has a `cluster` argument, which
is a context name. When a tool call leaves out `cluster`, the current context
is used.

### 2.1 Top-level fields

| Field      | Required | Description |
| ---------- | -------- | ----------- |
| `context`  | no       | Name of the default context. If it is missing, or that context is unusable, the first usable context (sorted by name) is used and a warning is logged. `--context` overrides it. |
| `contexts` | yes      | Map of context name → context. At least one context must be usable, or startup fails. |

### 2.2 Context fields

| Field       | Required | Secret | Description |
| ----------- | -------- | ------ | ----------- |
| `endpoints` | yes      | no     | Talos API (apid) addresses: IPs or hostnames, with an optional `:port` (default `50000`). The client tries them in turn. Usually the control plane nodes or a load balancer in front of them. |
| `nodes`     | no       | no     | Default target nodes for node tools when the call does not name a node. |
| `ca`        | yes      | no     | Base64-encoded PEM of the cluster's Talos API CA certificate. |
| `crt`       | yes      | no     | Base64-encoded PEM of the client certificate. The certificate's role is read from it (see 2.4). |
| `key`       | yes      | **yes** | Base64-encoded PEM of the client certificate's private key. May be stored encrypted (see 4). |
| `discovery` | no       | —      | Discovery service block (see 2.3). |

A context is **skipped with a warning** (the server still starts with the
others) when:

- it has no `endpoints`, or is missing `ca`, `crt` or `key`;
- it uses Omni authentication (`auth.siderov1`), which is not supported;
- `crt` cannot be parsed, or carries no known role (see 2.4);
- its encrypted fields cannot be decrypted.

If the skipped context is the one selected with `--context`, startup fails
instead. In `server` mode, `--require-all-contexts` makes any skipped context
fatal.

Other fields that `talosctl` writes (for example `cluster`) are ignored.

### 2.3 Discovery block

Talos nodes register themselves as *affiliates* with the Talos discovery
service. With the cluster ID and cluster secret, talos-mcp can read the node
list straight from the discovery service, without reaching the Talos API.
This still works when the cluster endpoints are down.

The block is used only by the `talos_clusters_members` tool. That tool is
registered only when at least one context has a valid `discovery` block.

| Field            | Required | Secret | Description |
| ---------------- | -------- | ------ | ----------- |
| `endpoint`       | no       | no     | Discovery service address. Default `discovery.talos.dev:443`. Accepted forms: `host`, `host:port`, `https://host/`, `https://host:port/`, and `http://host:port/` for a self-hosted service without TLS (default port 80). The `grpcs://` and `grpc://` schemes work as synonyms. A URL path is not allowed. |
| `cluster_id`     | yes      | no     | The cluster ID: `cluster.id` in the machine config or in the `talosctl gen secrets` bundle. |
| `cluster_secret` | yes      | **yes** | The cluster secret: `cluster.secret` in the machine config or the secrets bundle. Base64, must decode to a valid AES key (32 bytes). May be stored encrypted (see 4). |

`talos-mcp config import` (4.3) fills the whole block from a control plane
machine config.

A bad discovery block (missing field, bad secret, bad endpoint) disables
discovery for that context with a warning. The context itself stays usable.

The server only reads from the discovery service. It never registers itself
or writes affiliates. Treat `cluster_secret` like the private key anyway:
anyone who has it can read the node inventory and **forge affiliates**, which
KubeSpan nodes trust as WireGuard peers.

> **Note.** `talosctl config merge`, `talosctl config context` and other
> commands that rewrite the talosconfig drop the `discovery` block, because
> `talosctl` does not know it. Keep a separate talosconfig for talos-mcp, or
> add the block back after running them.

### 2.4 Credential role

The role is stored in the client certificate's Subject Organization (O). It
decides which tools are available for that cluster:

| Certificate role | Effective role | Tools |
| ---------------- | -------------- | ----- |
| `os:reader`      | reader         | all read-only tools |
| `os:operator`    | operator       | read-only tools, plus `talos_node_reboot` when `--allow-destructive` is set |
| `os:admin`       | operator       | same as `os:operator`, with a startup warning |
| none of these    | —              | context skipped with a warning |

When a certificate carries several roles, the highest wins. Unknown roles are
ignored. An expired certificate only produces a warning; the context is kept,
and its calls fail at the Talos API.

Use a dedicated credential with the smallest role that you need:

```sh
talosctl config new --roles os:reader   --crt-ttl 8760h mcp-reader-config
talosctl config new --roles os:operator --crt-ttl 8760h mcp-operator-config
```

The role is read from `crt`, which is never encrypted, so it can be checked
without unlocking anything.

## 3. Flags and environment variables

Every flag has an environment variable. A flag given on the command line wins
over its environment variable.

### 3.1 Locating and selecting the talosconfig

| Flag            | Env             | Default           | Description |
| --------------- | --------------- | ----------------- | ----------- |
| `--talosconfig` | `TALOSCONFIG`   | `~/.talos/config` | Path to the talosconfig. A leading `~/` is expanded. The file is never created or modified by the server. |
| `--context`     | `TALOS_CONTEXT` | all contexts      | Use only this one context. An unknown or unusable context is a startup error. The `config` subcommands ignore `TALOS_CONTEXT` and only honor `--context`. |
| `--require-all-contexts` | `REQUIRE_ALL_CONTEXTS` | `false` | `server` mode only. Fail to start when any context is skipped. |

### 3.2 Unlocking encrypted fields

| Flag | Env | Description |
| ---- | --- | ----------- |
| `--talosconfig-identity` | `TALOSCONFIG_IDENTITY` | Identity file that decrypts fields: an age identity file (`AGE-SECRET-KEY-1...`) or an OpenSSH private key (`ed25519` or `rsa`). Repeat the flag, or separate paths with commas in the env variable, to give several. |
| `--talosconfig-passphrase-file` | `TALOSCONFIG_PASSPHRASE_FILE` | File whose first line is the passphrase. The trailing newline (`\n` or `\r\n`) is removed. |
| `--talosconfig-askpass` | `TALOSCONFIG_ASKPASS` | Program that prints the passphrase on stdout. It gets the prompt text as its only argument, runs with no stdin, and is stopped after 60 seconds. Only the first line of its output is used. |
| — | `TALOSCONFIG_PASSPHRASE` | The passphrase itself. The server removes it from its own environment as soon as it reads it. Prefer the other sources where you can: environment variables can leak through `/proc/<pid>/environ`, crash dumps and child processes. |
| — (automatic) | — | Terminal prompt on `/dev/tty`. Used only by the `tools` and `config` subcommands, only when a terminal is available, and only when none of the passphrase sources above is set. **Never** used in `mcp` or `server` mode, because there stdin is the MCP transport or there is no user. |

How the sources are used:

- Identities and a passphrase source can be configured together. For each
  encrypted field, the identities are tried first, then the passphrase.
- When several passphrase sources are set, the first one wins in this order:
  passphrase file, askpass, `TALOSCONFIG_PASSPHRASE`, terminal prompt.
- An **encrypted OpenSSH key** can be used as an identity. Its passphrase is
  taken from the same passphrase sources, and is asked for only when the key
  is actually needed. The key's public part is read from the key file, or
  from `<path>.pub` next to it. `ssh-agent` is not supported.
- If the talosconfig has encrypted fields but no unlock source is set,
  startup fails with a message that lists the options.

### 3.3 Other server flags

These are not about the talosconfig, but are listed for completeness.

| Flag | Env | Default | Description |
| ---- | --- | ------- | ----------- |
| `--extensions` | `EXTENSIONS` | `all` | Tool groups to enable: `cluster`, `node`, or `all` (comma-separated). |
| `--allow-destructive` | `ALLOW_DESTRUCTIVE` | `false` | Enable destructive tools such as `talos_node_reboot` (operator role needed). |
| `--log-level` | `LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error`. |
| `--log-format` | `LOG_FORMAT` | `text` | `text` or `json`. |
| `--listen-address` | `LISTEN_ADDRESS` | `127.0.0.1:8080` | `server` mode: listen address as `host:port`. Use `:8080` to accept connections on every IPv4 and IPv6 address (dual stack). Write an IPv6 address in brackets, like `[::1]:8080`. The container image sets `:8080`. |
| `--disable-localhost-protection` | `DISABLE_LOCALHOST_PROTECTION` | `false` | `server` mode: accept a non-localhost `Host` header on a loopback address, as sent by a sidecar proxy. This disables DNS rebinding protection. |

Boolean environment variables accept `true`/`1`/`yes`/`on` and
`false`/`0`/`no`/`off`.

## 4. Encrypted secrets

`key` and `discovery.cluster_secret` can be stored **encrypted**, field by
field, much like a passphrase-protected SSH key. The public fields (`ca`,
`crt`, `endpoints`, `cluster_id`) stay readable, so the file can still be
reviewed and diffed, and the role can still be read from `crt`.

### 4.1 Format

Encrypted values use [age](https://age-encryption.org) in ASCII armor, then
base64, like every other talosconfig value:

```yaml
contexts:
  prod-eu:
    endpoints: [10.0.0.10]
    ca: LS0tLS1CRUdJTi...
    crt: LS0tLS1CRUdJTi...
    key: LS0tLS1CRUdJTiBBR0UgRU5DUllQVEVEIEZJTEUtLS0tLQ...            # encrypted
    discovery:
      cluster_id: 3x9y...
      cluster_secret: LS0tLS1CRUdJTiBBR0UgRU5DUllQVEVEIEZJTEUtLS0tLQ... # encrypted
```

A field is treated as encrypted when its base64-decoded value starts with
`-----BEGIN AGE ENCRYPTED FILE-----`. Anything else is plaintext. Plaintext
and encrypted fields, and plaintext and encrypted contexts, can be mixed in
one file.

A value is encrypted **either** with a passphrase **or** to one or more
recipients, never both (age allows a passphrase only as the sole recipient).
Recipients can be:

- age public keys (`age1...`);
- SSH public keys (`ssh-ed25519 ...`, `ssh-rsa ...`).

Several recipients can be used at once, for example the SSH keys of everyone
on the team plus an age key that belongs to a server deployment. Any one of
the matching private keys can decrypt the field.

### 4.2 What happens at startup

1. The talosconfig is parsed. Each context's role is read from `crt`.
2. If any field of the selected contexts is encrypted, the unlock material is
   loaded once, and **every encrypted field is decrypted at startup**. A wrong
   passphrase fails at once, not on the first tool call.
3. The decrypted values are kept only in memory. The passphrase and identity
   keys are wiped and dropped right after startup.
4. A context whose fields do not decrypt (no matching identity, or a wrong
   passphrase) is skipped with an error that names the context and field.
   The error never contains ciphertext or plaintext.
5. In `server` mode, a context with a plaintext `key` logs an info-level hint
   recommending encryption.

The file is read once. Changes need a restart.

Encryption protects the secrets **at rest** only: once decrypted they live
in the server's memory, the same as a plaintext talosconfig.

### 4.3 `talos-mcp config import`

Builds a context from a **control plane** machine config (for example the
`controlplane.yaml` from `talosctl gen config`) and adds it to a talosconfig.
It is the quickest way to get a complete context, discovery block included.

```sh
talos-mcp config import MACHINECONFIG -o OUTPUT
    [--context NAME] [--roles os:reader] [--crt-ttl 8760h]
    [-e ENDPOINT ...] [-n NODE ...] [--no-discovery] [--force]
    [--passphrase | --recipient KEY ... | --recipient-file FILE ...]
```

What it takes from the machine config:

| talosconfig field | Source |
| ----------------- | ------ |
| context name | `cluster.clusterName`, unless `--context` is set |
| `endpoints` | The host of `cluster.controlPlane.endpoint`, unless `--endpoints` is set |
| `ca` | `machine.ca.crt` |
| `crt`, `key` | A **new** client certificate with `--roles`, signed by `machine.ca.key`. The CA key itself is never written. |
| `discovery.endpoint` | The discovery service endpoint (`cluster.discovery.registries.service.endpoint` or a `DiscoveryServiceConfig` document) |
| `discovery.cluster_id`, `discovery.cluster_secret` | `cluster.id` and `cluster.secret` (or a `DiscoveryIdentityConfig` document) |

| Flag | Description |
| ---- | ----------- |
| `-o`, `--output` | **Required.** The talosconfig to add the context to. It is created when missing; otherwise the other contexts, comments and unknown keys are kept. Written atomically with mode `0600`. |
| `--context` | Context name. Default: the cluster name. |
| `--roles` | Roles of the client certificate. Default `os:reader`. Use `os:operator` to allow the operator tools. |
| `--crt-ttl` | Lifetime of the client certificate. Default `8760h` (one year). |
| `-e`, `--endpoints` | Talos API endpoints, instead of the cluster endpoint host. |
| `-n`, `--nodes` | Default nodes of the context. |
| `--no-discovery` | Leave out the discovery block. |
| `-f`, `--force` | Replace the context when it already exists. Without it, the command fails. |
| `--passphrase`, `-r`, `-R` | Encrypt `key` and `cluster_secret` before they are written, with the same rules as `config encrypt` (4.4). Without them, both are stored in plaintext. |

Rules:

- A worker machine config has no CA key and is refused.
- The discovery block is skipped, with a note, when discovery or the
  discovery service is disabled in the machine config.
- The current context of an existing file is kept. A new file gets the
  imported context as its current context.
- `-` reads the machine config from stdin.
- A symlinked `--output` is followed: the file it points to is updated and
  the link is kept.

Examples:

```sh
# Read-only context, encrypted to your SSH key, discovery included
talos-mcp config import controlplane.yaml -o ~/.talos/mcp-config \
    --recipient-file ~/.ssh/id_ed25519.pub

# Operator context under another name, added to an existing file
talos-mcp config import controlplane.yaml -o ~/.talos/mcp-config \
    --context prod-eu --roles os:operator -e 10.0.0.10,10.0.0.11 --passphrase

# From a running control plane node (needs an os:admin talosconfig)
talosctl -n 10.0.0.10 read /system/state/config.yaml | \
    talos-mcp config import - -o ~/.talos/mcp-config --recipient-file ~/.ssh/id_ed25519.pub
```

> **Warning.** The control plane machine config holds every cluster secret.
> Run the import where that file already lives, and do not copy it around
> just for this. The output holds only what talos-mcp needs.

### 4.4 `talos-mcp config encrypt`

Encrypts `key` and `cluster_secret` of every context (or of `--context`) and
writes the result to a new file.

```sh
talos-mcp config encrypt [--talosconfig FILE] -o OUTPUT
    (--passphrase | --recipient KEY ... | --recipient-file FILE ...)
    [--context NAME] [--field key,cluster_secret]
```

| Flag | Description |
| ---- | ----------- |
| `-o`, `--output` | **Required.** Output file. Written atomically with mode `0600`. It must not be the input file: the input is never overwritten. |
| `--passphrase` | Encrypt with a passphrase. It is asked twice on the terminal. It is implied when `--talosconfig-passphrase-file`, `--talosconfig-askpass` or `TALOSCONFIG_PASSPHRASE` is set and no recipient is given; the passphrase is then read from that source. |
| `-r`, `--recipient` | An age (`age1...`) or SSH (`ssh-ed25519 ...`, `ssh-rsa ...`) public key. Repeatable. |
| `-R`, `--recipient-file` | A file with recipients, one per line, such as `~/.ssh/id_ed25519.pub` or an `authorized_keys` file. Blank lines and `#` comments are skipped. Repeatable. |
| `--field` | Fields to encrypt: `key`, `cluster_secret`, or both (default). |
| `--context` | Encrypt only this context. |

Rules:

- `--passphrase` cannot be combined with `--recipient` or `--recipient-file`.
- Fields that are already encrypted are left as they are, so running the
  command again is safe (for example, to encrypt a context added later).
- Empty or missing fields are skipped.
- Comments, key order and unknown keys in the file are kept.
- The command prints how many fields it encrypted and how many were already
  encrypted.

Examples:

```sh
# Passphrase, asked twice on the terminal
talos-mcp config encrypt --talosconfig ~/.talos/mcp-config -o ~/.talos/mcp-config.enc --passphrase

# Passphrase from a file (no prompt)
talos-mcp config encrypt -o mcp-config.enc --talosconfig-passphrase-file ./passphrase.txt

# To your SSH key and a server's age key
talos-mcp config encrypt -o mcp-config.enc \
    --recipient-file ~/.ssh/id_ed25519.pub \
    --recipient age1qyqszqgpqyqszqgpqyqszqgpqyqszqgpqyqszqgpqyqszqgpqysq8lh2qp

# Only the key of one context
talos-mcp config encrypt -o mcp-config.enc --context prod-eu --field key --passphrase
```

To create an age key pair, use `age-keygen` from the age project:

```sh
age-keygen -o mcp-identity.txt     # prints the public key (age1...) on stderr
```

### 4.5 `talos-mcp config check`

Checks that every encrypted field decrypts with the configured unlock
sources, and prints one row per context:

```sh
$ talos-mcp config check --talosconfig mcp-config.enc --talosconfig-identity ~/.ssh/id_ed25519
CONTEXT   ROLE      ENCRYPTED           STATUS
prod-eu   reader    key,cluster_secret  ok
staging   operator  key                 ok
old       -         -                   ok
```

| Column      | Meaning |
| ----------- | ------- |
| `CONTEXT`   | Context name. |
| `ROLE`      | Effective role read from `crt` (`reader`, `operator`), or `-` when it cannot be read. |
| `ENCRYPTED` | Encrypted fields, or `-` for none. |
| `STATUS`    | `ok`, or `error: <field>: <reason>`. |

The command exits with an error when any context fails to decrypt. It does
not contact the cluster.

### 4.6 `talos-mcp config decrypt`

Prints the talosconfig with all encrypted fields decrypted, to **stdout
only**. Use it to get back a file `talosctl` can use, or before re-encrypting.

```sh
talos-mcp config decrypt --talosconfig mcp-config.enc --talosconfig-identity ~/.ssh/id_ed25519 > mcp-config
chmod 600 mcp-config
```

`--context` limits decryption to one context; the other contexts are printed
unchanged.

### 4.7 Rotating keys or the passphrase

Fields that are already encrypted are never re-encrypted, so rotate in two
steps:

```sh
umask 077
talos-mcp config decrypt --talosconfig mcp-config.enc --talosconfig-identity old-identity.txt > plain.yaml
talos-mcp config encrypt --talosconfig plain.yaml -o mcp-config.enc --recipient-file new-recipients.txt
rm plain.yaml
```

### 4.8 Compatibility with talosctl

- `talosctl` cannot use a context with an encrypted `key`; it fails with
  "failed to find any PEM data". Keep the encrypted file for talos-mcp only.
- `talosctl config` commands keep an encrypted `key` when they rewrite the
  file, but drop the whole `discovery` block.
- Plaintext talosconfigs work unchanged, and need no unlock flags.

## 5. Deployment recipes

### 5.1 Local, Claude Desktop / Claude Code (stdio)

Encrypt to your own SSH key, and point the server at the private key:

```json
{
  "mcpServers": {
    "talos": {
      "command": "talos-mcp",
      "args": ["mcp"],
      "env": {
        "TALOSCONFIG": "/home/me/.talos/mcp-config.enc",
        "TALOSCONFIG_IDENTITY": "/home/me/.ssh/id_ed25519"
      }
    }
  }
}
```

If the SSH key has a passphrase, add an askpass helper, since `mcp` mode
cannot prompt. For example, on macOS with the passphrase in the keychain:

```sh
#!/bin/sh
# ~/bin/talos-askpass
exec security find-generic-password -s talos-mcp -w
```

```json
"TALOSCONFIG_ASKPASS": "/home/me/bin/talos-askpass"
```

Other good askpass helpers: `pass show talos-mcp`, `op read op://vault/talos-mcp/password`
(1Password CLI), or a GUI prompt such as `ssh-askpass`.

### 5.2 Running tools from a shell

`talos-mcp tools` can prompt on the terminal, so a passphrase-encrypted file
works with no extra flags:

```sh
talos-mcp tools --talosconfig mcp-config.enc talos_clusters_list
Enter passphrase for talosconfig:
```

### 5.3 Kubernetes (server mode)

Encrypt the talosconfig to an age key that belongs to this deployment. The
encrypted file can then be kept in git or in a ConfigMap; the age identity
goes in a **separate** Secret, so reading the talosconfig alone is not enough
to get credentials, and RBAC can restrict the identity Secret on its own.

```sh
age-keygen -o identity.txt 2> recipient.txt   # recipient.txt: "Public key: age1..."
talos-mcp config encrypt --talosconfig mcp-config -o mcp-config.enc \
    --recipient "$(sed -n 's/^Public key: //p' recipient.txt)"

kubectl create configmap talos-mcp-talosconfig --from-file=talosconfig=mcp-config.enc
kubectl create secret generic talos-mcp-identity --from-file=identity.txt=identity.txt
```

Helm values for the chart:

```yaml
args:
  - --talosconfig=/etc/talos-mcp/talosconfig
  - --talosconfig-identity=/etc/talos-mcp-identity/identity.txt
  - --require-all-contexts

volumes:
  - name: talosconfig
    configMap:
      name: talos-mcp-talosconfig
  - name: identity
    secret:
      secretName: talos-mcp-identity
      defaultMode: 0400

volumeMounts:
  - name: talosconfig
    mountPath: /etc/talos-mcp
    readOnly: true
  - name: identity
    mountPath: /etc/talos-mcp-identity
    readOnly: true
```

To use a passphrase instead, store it in a Secret, mount it as a file, and
pass `--talosconfig-passphrase-file`.

## 6. Troubleshooting

| Message | Cause and fix |
| ------- | ------------- |
| `talosconfig has encrypted fields; set --talosconfig-identity, ...` | The file has encrypted fields but no unlock source is set. Add an identity, a passphrase file, an askpass program, or `TALOSCONFIG_PASSPHRASE`. |
| `context "x" skipped: key: ...` | That context's key did not decrypt with the given identities or passphrase. Run `talos-mcp config check` to see which. |
| `talosconfig has no usable contexts` | Every context was skipped. The warnings logged before it say why. |
| `unknown context "x": talosconfig has a, b` | `--context` (or `TALOS_CONTEXT`) names a context that does not exist. |
| `context "x": discovery disabled: ...` | The `discovery` block is incomplete or invalid. The context still works, without `talos_clusters_members`. |
| `client certificate has no os:reader, os:operator or os:admin role` | The certificate's role is not usable by talos-mcp. Generate a new one with `talosctl config new --roles os:reader`. |
| `context "x" uses an os:admin credential` | A warning: admin credentials work, with operator tools only. Use a dedicated `os:reader` or `os:operator` credential. |
| `encrypted key without a public key` | An encrypted OpenSSH private key was given as identity, but its public key is neither in the file nor in `<path>.pub`. Put the `.pub` file next to it. |
| `--passphrase needs a terminal, ...` | `config encrypt --passphrase` was run without a terminal. Use `--talosconfig-passphrase-file` or `--talosconfig-askpass`. |
| `askpass ...: timed out after 1m0s` | The askpass program did not finish within 60 seconds. |
| `failed to find any PEM data` (from talosctl) | talosctl was given an encrypted file. Use `talos-mcp config decrypt` to get a plaintext copy. |
