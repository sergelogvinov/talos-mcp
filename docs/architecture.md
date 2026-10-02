# Talos MCP Architecture

## 1. What is Talos MCP?

`talos-mcp` is a small server that lets AI agents look at and manage
[Talos Linux](https://www.talos.dev) clusters. It speaks the Model Context
Protocol (MCP), so any MCP client (Claude Desktop, Claude Code, Cursor,
VS Code, scripts) can use it.

The server talks to the Talos API (apid) on the cluster nodes. It turns many
low-level API calls into a few simple **tools** that an agent can call.

It is a single Go binary with no database and no extra services.

## 2. Main ideas

- **Read-only by default.** Almost every tool only reads data. The one tool
  that changes something (node reboot) exists only when you start the server
  with `--allow-destructive`.
- **talosconfig is the only source of truth.** Clusters, endpoints, nodes and
  credentials all come from a normal `talosconfig` file, the same file that
  `talosctl` uses. There is no other cluster config.
- **Useful, combined results.** A tool does not copy one Talos API call. It
  collects data from several calls and returns one short summary that an
  agent can understand.
- **Typed output.** Every tool returns structured data (JSON with a schema)
  and also a short text version for people and language models.
- **Safe output.** Logs and messages are cleaned of secrets before they leave
  the server.

## 3. Big picture

```
┌──────────────────────────────────────────────────────────────┐
│                         MCP Client                           │
│        (Claude Desktop/Code, Cursor, VS Code, scripts)       │
│        stdio (pipe)  or  streamable HTTP (/mcp)              │
└───────────────────────────┬──────────────────────────────────┘
                            │ JSON-RPC 2.0
                            ▼
┌──────────────────────────────────────────────────────────────┐
│                  talos-mcp (single Go binary)                │
│                                                              │
│  cmd/talos-mcp      commands: mcp | server | tools |         │
│        │                      config | version               │
│  internal/tools     MCP tools (cluster / node)               │
│        │                                                     │
│  internal/talos     client pool, node lookup,                │
│        │            discovery service reader                 │
│        │                                                     │
│  internal/config    flags, talosconfig loading, secrets      │
└──────────────┬────────────────────────────────┬──────────────┘
               │ gRPC + mTLS                    │ gRPC/TLS, read only
               ▼                                ▼
┌───────────────────────────────┐  ┌───────────────────────────────┐
│ Talos apid on endpoints       │  │ Discovery service             │
│ (control plane nodes or LB),  │  │ (discovery.talos.dev or       │
│ forwards calls to the         │  │  self-hosted): list of        │
│ target nodes                  │  │  cluster members              │
└───────────────────────────────┘  └───────────────────────────────┘
```

How a request flows:

1. The MCP client calls a tool, for example `talos_node_logs`.
2. The tool checks the input and checks that the cluster's credential is
   allowed to use this tool.
3. The tool gets a Talos client for the cluster from the **client pool**.
4. The client connects to the cluster **endpoints** (usually control plane
   nodes or a load balancer). apid on the endpoint forwards the call to the
   **target node**.
5. The tool cleans and shortens the result, and returns it as structured data
   plus text.

## 4. Configuration

All settings come from two places:

- **Command-line flags and environment variables.** Every flag also has an
  environment variable.
- **The talosconfig file.** It is found in this order: the `--talosconfig`
  flag, the `TALOSCONFIG` variable, then `~/.talos/config`. The server only
  reads this file. It never creates or changes it.

**One talosconfig context is one cluster.** Every tool has an optional
`cluster` argument, which is a context name. If it is empty, the current
context is used.

Each context can also have an optional `discovery` block (cluster ID and
cluster secret). It allows the server to read the member list from the Talos
discovery service.

The private key and the cluster secret can be stored **encrypted** in the
file (with age: a passphrase, an age key or an SSH key). The server decrypts
them in memory at startup.

At startup the server checks each context. A broken context is skipped with
a warning, so one bad context does not stop the server.

For all fields, flags, encryption and deployment examples, see
[config.md](config.md).

## 5. Credential roles

Each context has exactly one client certificate. Talos stores the role in
the certificate. The server reads it at startup, without any network call,
and uses it to decide which tools are available for that cluster:

| Certificate role | Effective role | Tools                                                  |
| ---------------- | -------------- | ------------------------------------------------------ |
| `os:reader`      | reader         | all read-only tools                                    |
| `os:operator`    | operator       | read-only tools + `talos_node_reboot` (with the flag)  |
| `os:admin`       | operator       | same as operator, with a warning                       |
| other            | —              | context skipped                                        |

Access is checked at three levels. All three must allow a call:

1. **At startup:** a tool is registered only if at least one cluster can use
   it. If a tool is not registered, the client cannot see it at all.
2. **On every call:** the tool checks the role of the chosen cluster before
   it makes any Talos call.
3. **In Talos:** apid itself checks the certificate role. So even a bug in
   the server cannot do more than the certificate allows.

Best practice: create a separate `os:reader` (or `os:operator`) credential
just for talos-mcp.

## 6. Code layout

```
cmd/talos-mcp/       commands, flags, HTTP server, wiring
internal/config/     flags → Config, talosconfig loading and checks,
                     decryption of encrypted fields
internal/secrets/    age encryption and unlock sources (passphrase, keys)
internal/logger/     structured logging (slog)
internal/server/     in-memory MCP client, used by the `tools` command
internal/talos/      client pool, roles, node lookup, discovery reader
internal/tools/      one file per MCP tool, registration, schemas
internal/utils/      log sanitizer (removes secrets)
pkg/formatter/       turns result structs into compact text
charts/talos-mcp/    Helm chart for server mode
manifest.json        MCPB bundle manifest
```

Layer rules:

- `cmd/talos-mcp` only reads flags, builds the pool and starts the
  transport. It has no Talos logic.
- `internal/tools` uses `internal/talos`, but never reads the talosconfig or
  opens gRPC connections itself.
- `internal/talos` uses `internal/config` and the Talos `pkg/machinery`
  module. It does not use the full Talos module, to keep dependencies small.

## 7. Main components

### 7.1 Commands

| Command             | Purpose                                                   |
| ------------------- | --------------------------------------------------------- |
| `talos-mcp mcp`     | MCP over stdio, for local clients                         |
| `talos-mcp server`  | MCP over streamable HTTP on `/mcp`, plus `/healthz`       |
| `talos-mcp tools`   | Run one tool from the shell, output as text, JSON or YAML |
| `talos-mcp config`  | `import` a context from a machine config; `encrypt`, `decrypt` or `check` the talosconfig secrets |
| `talos-mcp version` | Print the version                                         |

The `tools` command uses an in-memory MCP client, so it runs the same code
path as a real client.

### 7.2 Client pool (`internal/talos`)

The pool is the center of the server. It holds:

- the list of usable contexts and the current one;
- the role and certificate expiry of each context;
- the discovery settings of each context;
- one Talos client per context.

Clients are created on first use and then reused. Creating a client does not
connect yet, so an unreachable cluster never blocks startup. If creation
fails, nothing is cached and the next call tries again.

Every tool call has a time limit: 30 seconds, or 60 seconds for tools that
query many nodes.

### 7.3 Node lookup

Node tools take a `node` argument. It can be:

- an IP address, used as it is;
- a hostname or node name, which the pool looks up in the cluster member
  list;
- empty, which uses the single default node from the context (if there is
  exactly one).

The member list comes from the Talos API (the COSI `Members` resource). If
that fails, the pool falls back to the `nodes` and `endpoints` from the
talosconfig and adds a warning. The tool tells the agent which source it
used.

When a node has several addresses, the pool picks the one that is most
likely to work. It never uses link-local addresses.

### 7.4 Discovery service reader

Talos nodes register themselves with the discovery service. The records are
encrypted with the cluster secret. With the cluster ID and secret, the server
can read the member list **without the Talos API**. This is helpful when the
endpoints are down.

- It only reads. It never registers itself or writes records.
- Results are cached for 30 seconds per cluster.
- Only one tool uses it: `talos_clusters_members`.

The cluster secret is sensitive. Anyone who has it can also write fake
members, which KubeSpan nodes trust. Keep it as safe as the private key.

### 7.5 Tools (`internal/tools`)

Each tool lives in its own file. It has typed input and output structs, and
the MCP SDK builds JSON schemas from them. The schemas are adjusted so that
clients with simple schema support (for example Gemini) also accept them.

| Tool                      | Group   | Min role | What it does                                                   |
| ------------------------- | ------- | -------- | -------------------------------------------------------------- |
| `talos_clusters_list`     | cluster | reader   | Lists configured clusters, roles and tools. No network calls.  |
| `talos_clusters_describe` | cluster | reader   | Health and inventory of all nodes: versions, uptime, services, etcd. |
| `talos_clusters_event`    | cluster | reader   | Recent runtime events on all nodes or one node.                |
| `talos_clusters_members`  | cluster | reader   | Member list from the discovery service. Only with discovery keys. |
| `talos_node_describe`     | node    | reader   | One node in detail: resources, services, events, last logs.    |
| `talos_node_logs`         | node    | reader   | Tail of a Talos service log or a Kubernetes container log.     |
| `talos_node_dmesg`        | node    | reader   | Tail of the kernel log.                                        |
| `talos_node_reboot`       | node    | operator | Reboots one node. Only with `--allow-destructive`.             |

`--extensions` can turn on only the `cluster` or `node` group.

Tools that query many nodes (`describe`, `event`) ask at most 8 nodes at the
same time. If some nodes fail, the tool still returns a result with warnings.
It fails only when no node answers.

The reboot tool has extra safety rules:

- `cluster` and `node` are required, and only one node per call.
- Before it reboots a control plane node, it checks etcd quorum. If the
  reboot could break quorum, or the check cannot run, it refuses.
- It returns as soon as Talos accepts the request. The agent can then follow
  progress with `talos_clusters_event`.

## 8. Output and safety

- Every log, dmesg and event line goes through the **sanitizer**. It hides
  tokens, passwords, keys, certificates and Talos secrets. The configured
  cluster secrets are always hidden too.
- `grep` filters work on the cleaned text, so they cannot be used to guess a
  hidden value.
- Output size is limited: at most 1000 lines, and each line is cut at 4 KiB.
  A `truncated` flag tells the agent when something was cut.
- Errors are short and helpful. For a wrong input, the error lists the valid
  choices (cluster names, node names, services), so the agent can fix the
  call at once.

## 9. Running and deployment

- **Local (stdio):** the MCP client starts `talos-mcp mcp` and uses the
  user's own talosconfig.
- **HTTP server:** `talos-mcp server` listens on `127.0.0.1:8080` by default.
  The HTTP endpoint has **no authentication**, and all callers share the same
  talosconfig credentials. Protect it at the network or ingress level.
- **Kubernetes:** the Helm chart runs server mode. The talosconfig comes from
  a mounted Secret or ConfigMap, and destructive tools are off by default.
- **MCPB bundle:** `manifest.json` runs the binary in `mcp` mode.
- **Logging:** every tool call is logged with its name and arguments.
- **Shutdown:** on SIGINT or SIGTERM the server stops new requests, gives
  running calls a few seconds to finish, then closes all connections and the
  client pool.

## 10. Testing

- Unit tests use fake Talos clients and a fake discovery service, so they
  need no real cluster.
- Tests cover config loading, roles, node lookup, the sanitizer, each tool,
  and which tools are registered for each setup.
- Every tool must have valid input and output schemas.
- Optional integration tests create a real Talos cluster in Docker and run
  each tool through `talos-mcp tools`.
