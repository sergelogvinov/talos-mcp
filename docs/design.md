# Talos MCP Server — Design

## Go-based Local Daemon with stdio + streamable HTTP, Typed Structured Output for Every Tool

`talos-mcp` is a small MCP server that lets AI agents inspect and operate
[Talos Linux](https://www.talos.dev) clusters through the Talos API (apid).
It follows the same layout and conventions as `proxmox-mcp` and `mimiops-mcp`.

Design principles:

- **Read-only by default.** Nearly every tool is read-only. Destructive tools
  are only registered when the server runs with `--allow-destructive`.
- **talosconfig is the only source of truth.** Clusters, endpoints, target
  nodes and mTLS credentials all come from a standard `talosconfig` file.
  There is no separate cluster YAML.
- **Rich, aggregated output.** Tools don't mirror the Talos gRPC API one call
  at a time. Each tool combines several API calls into a compact result that
  an agent can reason about.
- **Typed structured output.** Every tool returns a typed Go struct as
  `StructuredContent`, plus a human/LLM-readable text rendering
  (`pkg/formatter`).

## 1. Architectural Overview

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
│  cmd/talos-mcp      cobra: mcp | server | tools | version    │
│        │                                                     │
│  internal/tools     MCP tool handlers (cluster / node)       │
│        │                                                     │
│  internal/talos     client pool: one Talos client per        │
│        │            talosconfig context, node resolution,    │
│        │            discovery service reader (read-only)     │
│        │                                                     │
│  internal/config    flags → Config, talosconfig loading      │
└──────────────┬────────────────────────────────┬──────────────┘
               │ gRPC + mTLS                    │ gRPC/TLS, List only
               │ (talosconfig certs)            │ (cluster_id; decrypt
               ▼                                ▼  with cluster_secret)
┌───────────────────────────────┐  ┌───────────────────────────────┐
│ Talos apid on endpoints       │  │ Discovery service             │
│ (control plane nodes / LB)    │  │ (discovery.talos.dev or       │
│ → proxied to target nodes via │  │  self-hosted): affiliates =   │
│   gRPC metadata "nodes"       │  │  node inventory               │
└───────────────────────────────┘  └───────────────────────────────┘
```

Talos routes requests through **endpoints**, which are usually control plane
nodes or a load balancer. apid then proxies each request to the **target
node(s)** named in the gRPC metadata. The server keeps this model: a tool
connects to the context's endpoints and targets nodes with
`client.WithNode(ctx, node)`.

## 2. Configuration

### 2.1 talosconfig

The server reads a standard talosconfig, the same file `talosctl` uses:

```yaml
context: prod-eu
contexts:
  prod-eu:
    endpoints: [10.0.0.10, 10.0.0.11, 10.0.0.12]
    nodes: [10.0.0.10]            # optional default target node(s)
    ca: LS0t...                    # base64 PEM
    crt: LS0t...
    key: LS0t...
  staging:
    endpoints: [staging-api.example.com]
    ca: LS0t...
    crt: LS0t...
    key: LS0t...
```

**One context is one cluster.** The `cluster` argument of every tool is a
talosconfig context name. When a tool omits `cluster`, the talosconfig's
current `context:` is used.

talosconfig path resolution, first match wins:

1. `--talosconfig` flag
2. `TALOSCONFIG` environment variable
3. `$HOME/.talos/config` (the `talosctl` default)

The server reads the file with `os.ReadFile` and parses it with
`clientconfig.FromBytes` from
`github.com/siderolabs/talos/pkg/machinery/client/config`. It does not use
`clientconfig.Open`, because `Open` creates the file when it is missing.

Startup validation:

- The file must parse and contain at least one usable context, or startup
  fails.
- Each context must have at least one endpoint and `ca`/`crt`/`key`. A context
  that doesn't is **skipped with a warning**, so one stale context in a
  developer's talosconfig doesn't stop the server.
- Contexts that use `auth.siderov1` (Omni) are **skipped with a warning** in
  v1. Omni auth needs interactive key signing (see §13).
- `--context` (optional) limits the server to a single context. This is useful
  when the server should only ever see one cluster. An unknown or unusable
  `--context` is a startup error, not a warning.
- The current context is `--context` when set, else the talosconfig's
  `context:`. If that one is missing or was skipped, the first usable context
  by name is used, with a warning.

### 2.2 Discovery service

Talos nodes register themselves with the
[discovery service](https://www.talos.dev/latest/talos-guides/discovery/)
(`discovery.talos.dev` by default, or a self-hosted instance) as **affiliates**.
Each affiliate record holds the node ID, hostname, node name, machine type,
OS version and addresses. The records are stored under the cluster ID and
encrypted with the cluster secret. With those two values the server can read
the node list **straight from the discovery service, without reaching apid**.
That still works when the Talos endpoints are down or unreachable, which is
when an agent most needs to know which nodes exist.

Discovery is used by **one tool only**, `talos_clusters_members` (§8.4).
Other tools, including `talos_clusters_describe` and node resolution, never
read the discovery service. The members tool is registered only when at
least one context has a valid `discovery` block (§9). Without keys there is
no tool.

The values go in an optional `discovery` block on each talosconfig context:

```yaml
context: prod-eu
contexts:
  prod-eu:
    endpoints: [10.0.0.10, 10.0.0.11, 10.0.0.12]
    ca: LS0t...
    crt: LS0t...
    key: LS0t...
    discovery:
      endpoint: discovery.talos.dev:443   # optional, this is the default; https://host/ and http://host:port also work
      cluster_id: 3x9y...                 # machine config cluster.id
      cluster_secret: c2VjcmV0...         # machine config cluster.secret (base64, 32 bytes)
```

You can find `cluster_id` and `cluster_secret` in the machine config
(`cluster.id`, `cluster.secret`) or in the `talosctl gen secrets` bundle
(`cluster.id`, `cluster.secret`).

Parsing:

- `clientconfig` doesn't know about the `discovery` key. Its YAML decoder is
  not strict, so it ignores the key (checked in `pkg/machinery` v1.14.2, and
  covered by a test). The server decodes the same bytes a second time with a
  small YAML struct
  (`contexts: map[string]struct{ Discovery *DiscoveryConfig }`) to pick it up.
- Validation: if `discovery` is present, `cluster_id` and `cluster_secret` are
  required, and the secret must base64-decode to a valid AES key. `endpoint`
  accepts `host`, `host:port`, or a URL in the machine-config form
  (`https://discovery.talos.dev/`), and is normalized to `host:port` (443 by
  default). An `http://` URL selects plain gRPC without TLS, for self-hosted
  services; other schemes and URL paths are rejected. A private CA is not
  supported in v1. A bad block
  disables discovery for that context with a warning. It does not disable the
  context.
- Caveat: `talosctl config merge`, `config context` and similar commands
  rewrite talosconfig through `clientconfig` and **drop** unknown keys. Keep
  the MCP talosconfig as a separate file (the default for Secret-mounted
  deployments), or add the block again after running those commands.

How the server uses it (`internal/talos/discovery.go`):

- It is a **read-only client**. It calls the discovery API `List` RPC
  (`github.com/siderolabs/discovery-api`, `server/pb.ClusterClient.List`) for
  the cluster ID, then decrypts each affiliate's data with AES-GCM keyed by
  the cluster secret and unmarshals it into the affiliate proto. The server
  **never calls `Hello`, `AffiliateUpdate` or `Watch`**, and it never
  registers itself as an affiliate. The cipher scheme is the one in
  `github.com/siderolabs/discovery-client` (`pkg/client`, `parseReply`):
  affiliate data is AES-GCM with a random 12-byte nonce prefix, and each
  endpoint record is AES-ECB over `[len][proto][zero padding]`. That code is
  not exported and `discovery-client` is MPL-2.0, so the derived part lives
  in its own file, `discovery_cipher.go`, under the MPL-2.0 header with
  attribution (MPL is per-file; the rest of the package stays Apache-2.0).
- The connection uses TLS with the system roots, like Talos nodes do.
- Each cluster has its own cache entry and lock, so a slow discovery service
  for one cluster never delays another, and a caller waiting on the lock
  gives up when its context ends. The cached list is copied for each caller.
- Results are cached per context for 30s, so a burst of tool calls doesn't
  hit the service repeatedly.

Security: the cluster secret decrypts all affiliate data. It could also be used
to **write** forged affiliates, which matters when KubeSpan is in use, because
nodes trust affiliates as WireGuard peers. Treat the secret like the client
key: keep it only in the Secret-mounted talosconfig, never log it, and never
return it in a tool result. The server's read-only use of the API means it
never needs write access.

### 2.2.1 Encrypted secrets

`key` and `cluster_secret` may be stored **encrypted** (age: passphrase,
age identity, or SSH key), similar to a passphrase-protected SSH key. The
server decrypts them in memory at startup. See [`secrets.md`](secrets.md).

### 2.3 Credential role → accessible tools

Each talosconfig context holds **exactly one credential**, and that
credential's Talos role decides which tools the server exposes for that
cluster. Talos stores the role in the client certificate's Subject
Organization (O). At startup, the server parses each context's `crt` with
`crypto/x509` and works out the role from it, without making any network
call:

| Certificate roles (O)             | Effective role | Tools for the cluster                                    |
| --------------------------------- | -------------- | -------------------------------------------------------- |
| `os:reader`                       | `reader`       | all read-only tools                                      |
| `os:operator`                     | `operator`     | read-only tools + `talos_node_reboot` (with `--allow-destructive`) |
| `os:admin`                        | `operator`     | same as `os:operator`, plus a startup warning (see below) |
| none of the above (e.g. only `os:etcd:backup`) | —  | context **skipped**, with a startup warning              |

- If a certificate has several roles, the highest one wins. Unknown roles are
  ignored.
- `os:admin` is accepted so a developer's default talosconfig still works
  with `talos-mcp mcp`. The server never calls an admin-only API, so it gives
  admin credentials the operator tool set and logs a warning recommending a
  dedicated `os:operator` or `os:reader` credential. This matters most in
  `server` mode, where the credential is shared.
- A context whose `crt` can't be parsed is skipped with a warning, the same
  as one with no known role. If it was the current context, the first
  remaining context becomes current. With `--context`, either case is a
  startup error.
- The certificate is also checked for expiry (§11). An expired certificate
  only produces a warning; the context stays, and its calls fail in apid.

Each tool declares the minimum role it needs:

| Tool                      | Min role   |
| ------------------------- | ---------- |
| `talos_clusters_list`     | reader     |
| `talos_clusters_describe` | reader     |
| `talos_clusters_event`    | reader     |
| `talos_clusters_members`  | reader     |
| `talos_node_logs`         | reader     |
| `talos_node_dmesg`        | reader     |
| `talos_node_reboot`       | operator   |

`talos_clusters_members` uses the discovery service, which doesn't use the
certificate at all. It also needs discovery keys on the context (§2.2), and
it is not available on a context without them.

The read-only roles were checked against the apid authorization rules in
Talos `main` (`internal/app/machined/pkg/system/services/machined.go`):
`Logs`, `LogsContainers`, `Dmesg`, `Events`, `ServiceList`, `Version`,
`SystemStat`, `Memory`, `EtcdMemberList` and COSI `Get`/`List`/`Watch` all
allow `os:reader`. `Reboot` needs `os:operator` or `os:admin`. Check this
again when you pin a `pkg/machinery` version (§15.1). If a release changes a
rule, this table is the only place that changes.

How the table is enforced is described in §9. Recommended credentials:

| Use                                 | Generate with                                      |
| ----------------------------------- | -------------------------------------------------- |
| Read-only (default)                 | `talosctl config new --roles os:reader ro.yaml`    |
| Reboot allowed                      | `talosctl config new --roles os:operator op.yaml`  |

### 2.4 Config struct

```go
// internal/config
type Config struct {
    TalosConfig      string // resolved path
    Context          string // optional: restrict to one context
    Port             int
    Extensions       string
    AllowDestructive bool
    LogLevel         string
    LogFormat        string
}
```

## 3. CLI Interface

Commands are built with `github.com/spf13/cobra`. Flags are defined with
`github.com/spf13/pflag`, and each flag has an environment variable fallback,
the same way as `proxmox-mcp/cmd/proxmox-mcp/flags.go`.

```
talos-mcp mcp       [global flags]               # stdio transport
talos-mcp server    [global flags] --port 8080   # streamable HTTP on /mcp, /healthz
talos-mcp tools     [global flags] [-o text|json|yaml] [tool] [key=value ...]
talos-mcp version
talos-mcp config    encrypt|decrypt|check     # encrypted secrets, see secrets.md
```

| Flag                  | Env                 | Default            | Scope      |
| --------------------- | ------------------- | ------------------ | ---------- |
| `--talosconfig`       | `TALOSCONFIG`       | `~/.talos/config`  | persistent |
| `--context`           | `TALOS_CONTEXT`     | "" (all contexts)  | persistent |
| `--allow-destructive` | `ALLOW_DESTRUCTIVE` | `false`            | persistent |
| `--extensions`        | `EXTENSIONS`        | `all`              | persistent |
| `--log-level`         | `LOG_LEVEL`         | `info`             | persistent |
| `--log-format`        | `LOG_FORMAT`        | `text`             | persistent |
| `--port`              | `PORT`              | `8080`             | `server`   |
| `-o, --output`        | —                   | `text`             | `tools`    |

`--extensions` selects tool groups (`cluster`, `node`, or `all`). It is kept
for parity with the sibling projects and lets a deployment expose only part of
the tool set.

Examples:

```sh
# Local, read-only, current talosconfig
talos-mcp mcp

# Remote HTTP server with reboot enabled, single cluster
talos-mcp server --talosconfig /etc/talos/config --context prod-eu --allow-destructive

# Ad-hoc invocation from the shell (no MCP client needed)
talos-mcp tools talos_node_logs cluster=prod-eu node=10.0.0.21 service=kubelet tail=50
talos-mcp tools -o json talos_clusters_describe
```

## 4. Project Structure

```
cmd/talos-mcp/
    main.go          entrypoint, root command, logging middleware, version/commit ldflags
    flags.go         Flags, env defaults, pflag registration
    mcp.go           `mcp` subcommand (stdio)
    server.go        `server` subcommand (streamable HTTP, /healthz, graceful shutdown)
    tools.go         `tools` subcommand (in-memory MCP client, key=value args)
    version.go       `version` subcommand
    config.go        `config encrypt|decrypt|check` subcommands (secrets.md)

internal/
    config/          Config, talosconfig path resolution & validation
    logger/          slog setup, context injection (copied from proxmox-mcp)
    server/          in-memory MCP client: ListTools / CallTool (copied from proxmox-mcp)
    talos/           Pool: per-context Talos clients, node resolution,
                     discovery service reader (discovery.go)
    tools/           tool handlers + Register*, typed results
        register.go
        types.go
        helpers.go
        clusters_list.go
        clusters_describe.go
        clusters_event.go
        clusters_members.go
        node_logs.go
        node_dmesg.go
        node_reboot.go
    utils/           log sanitizer (port of mimiops internal/utils/sanitize.go)

pkg/
    formatter/       struct → text rendering (shared with sibling projects)

docs/
    design.md        this document
charts/talos-mcp/    Helm chart (server mode)
manifest.json        MCPB bundle manifest
```

### Layering rules

- `cmd/talos-mcp` only parses flags, builds the pool and wires transports. It
  contains no Talos logic.
- `internal/tools` depends on `internal/talos`, `internal/utils`,
  `pkg/formatter` and the MCP SDK. It never reads talosconfig or builds gRPC
  connections directly.
- `internal/talos` depends on `internal/config`, Talos `pkg/machinery` and
  `discovery-api`.
- Destructive tools are registered only when `allowDestructive` is true. If
  the flag is off they don't exist for the client at all. There is no runtime
  "are you sure" check.

## 5. Core Dependencies

```go
module github.com/sergelogvinov/talos-mcp

go 1.27

require (
    github.com/modelcontextprotocol/go-sdk          // MCP server, transports, AddTool generics
    github.com/siderolabs/talos/pkg/machinery       // client, clientconfig, API protos, COSI resources
    github.com/cosi-project/runtime                 // COSI state access (safe.StateList etc.)
    github.com/siderolabs/discovery-api             // discovery service gRPC API + affiliate protos
    github.com/spf13/cobra                          // command dispatch
    github.com/spf13/pflag                          // flags
    go.yaml.in/yaml/v3                              // `tools -o yaml`
    github.com/stretchr/testify                     // tests
)
```

Use only `pkg/machinery`, which is a separate Go module, and never the full
`github.com/siderolabs/talos` module. This keeps the dependency tree small.

## 6. Talos Client Pool (`internal/talos`)

```go
type Pool struct {
    cfg       *clientconfig.Config
    contexts  []string                    // sorted, after --context filter & validation
    current   string                      // default context
    creds     map[string]*Credential      // per context, from the certificate (§2.3)
    discovery map[string]*config.DiscoveryConfig // per context, absent when not configured
    warnings  []string                    // startup warnings, logged by the caller

    newClient ClientFactory               // client.New by default, a fake in tests

    mu         sync.Mutex
    clients    map[string]Client           // lazily created, one per context
    affiliates map[string]cachedAffiliates // discovery service results, 30s TTL
}

func LoadPool(cfg *config.Config, opts ...Option) (*Pool, error)     // LoadTalosConfig + NewPool
func NewPool(tc *config.TalosConfig, opts ...Option) (*Pool, error)  // runs CheckCredentials (§2.3)
func (p *Pool) Warnings() []string
func (p *Pool) List() []string
func (p *Pool) Current() string
func (p *Pool) Resolve(cluster string) (string, error)              // "" → current; unknown → error listing valid names
func (p *Pool) Client(ctx context.Context, cluster string) (Client, error) // small interface (§14), fake in tests
func (p *Pool) ContextInfo(cluster string) (endpoints, nodes []string, err error)
func (p *Pool) Credential(cluster string) *Credential               // role, admin flag, NotAfter (cert_expires)
func (p *Pool) Discovery(cluster string) *config.DiscoveryConfig    // nil without a discovery block
func (p *Pool) Members(ctx context.Context, cluster string) (*MemberList, error)   // apid only, §6.1 order; members, source, warnings
func (p *Pool) ResolveNode(ctx context.Context, cluster, node string) (*ResolvedNode, error) // §6.1: IP, hostname, empty → default node
func (p *Pool) Affiliates(ctx context.Context, cluster string) (*AffiliateList, error) // discovery service, §8.4 only
func (p *Pool) Role(cluster string) Role                              // reader | operator
func (p *Pool) ClustersWithRole(min Role) []string                    // sorted; drives tool registration
func (p *Pool) ClustersWithDiscovery() []string                       // sorted; drives talos_clusters_members registration
func (p *Pool) Require(cluster string, min Role) error                // per-call role check (§9)
func (p *Pool) Close() error
```

- Clients are created lazily on first use with
  `client.New(ctx, client.WithConfig(cfg), client.WithContextName(name))`.
  `client.New` doesn't dial (it uses `grpc.NewClient`), so creating a client
  never blocks on an unreachable endpoint. `Client` is an interface: the
  real client is wrapped so COSI state is reachable as `State()`.
  They are cached for the server's lifetime and closed on shutdown. gRPC
  reconnects on its own, so a dead endpoint doesn't poison the cache.
- If client creation fails, nothing is cached, so the next call retries.
- Every tool call wraps its context in a timeout: 30s by default, 60s for
  aggregated calls like `talos_clusters_describe`.

### 6.1 Node resolution

The node tools take a `node` argument. Accepted forms:

1. an IP address, used as-is;
2. a hostname or node name, resolved to an address through
   `Pool.Members` (below);
3. empty, which falls back to the context's single default `nodes` entry. If
   the context lists zero or several nodes, the tool returns an error that
   names the known members so the agent can pick one.

`Pool.Members` returns the node list for a cluster. It **never uses the
discovery service**: the node tools need apid anyway, so discovery would add
nothing here. It tries these sources in order, and the first one that returns
nodes wins:

| # | Source                         | When used                                    | Needs apid |
| - | ------------------------------ | -------------------------------------------- | ---------- |
| 1 | COSI `Members.cluster.talos.dev` read through the endpoints | always tried first  | yes        |
| 2 | context `nodes`, then `endpoints` | source 1 failed or returned no nodes      | no (static) |

Source 1 returns hostname, addresses, machine type (controlplane/worker) and
OS version. Source 2 returns bare addresses with an unknown role. The source
that was used goes back to the tool as `MemberSource` (`members` or
`talosconfig`), and tools show it so the agent knows how much to trust the
list. If source 1 fails, the pool uses source 2 and adds a warning. It does
not fail the call.

Choosing the target address from a member's address list: never a
link-local address; prefer the same IP family as the context endpoints (DNS
endpoints count as IPv4), then any other address; an address that looks like
a KubeSpan or SideroLink ULA (`network.IsULA` from `pkg/machinery`) is the
last resort. `IsULA` only checks two bytes, so an ordinary site ULA subnet
can look like KubeSpan; such a node stays reachable that way.

Matching is case-insensitive against the hostname, the Kubernetes node name
(the Members resource ID), the node ID and, for the talosconfig source, the
configured address or DNS name. A `:port` suffix and IPv6 brackets in `node`
are ignored. Several matches are an "ambiguous node" error that lists each
match with its node ID and addresses. When the list came from the
talosconfig fallback, "unknown node" errors say why apid was not used.

## 7. Tool Registration & Structured Output

The pattern is the same as proxmox-mcp: one file per tool, with a
`Register<Name>(srv)` method and a handler on `TalosTools`. The handler calls
an exported method that the `tools` CLI and unit tests can reuse.

```go
type TalosTools struct {
    pool             *talos.Pool
    allowDestructive bool
    extensions       map[string]bool
    registered       []toolSpec // name, min role, needs discovery; filled by Register*
}

func (t *TalosTools) RegisterTools(srv *mcp.Server) {
    if t.enabled("cluster") {
        t.RegisterClustersList(srv)
        t.RegisterClustersDescribe(srv)
        t.RegisterClustersEvent(srv)
        // Discovery tool needs at least one context with discovery keys (§9).
        if len(t.pool.ClustersWithDiscovery()) > 0 {
            t.RegisterClustersMembers(srv)
        }
    }
    if t.enabled("node") {
        t.RegisterNodeLogs(srv)
        t.RegisterNodeDmesg(srv)
        // Destructive tools need the flag AND at least one operator cluster (§9).
        if t.allowDestructive && len(t.pool.ClustersWithRole(talos.RoleOperator)) > 0 {
            t.RegisterNodeReboot(srv)
        }
    }
}
```

Handlers return
`(&mcp.CallToolResult{Content: text(formatter.ToText(result))}, *result, nil)`.
Input structs use `json` and `jsonschema` tags, and the SDK generates the input
schema from them. Output structs are exported from `internal/tools` so the SDK
can generate output schemas.

Annotations:

| Tool                      | ReadOnly | Destructive | Idempotent | OpenWorld |
| ------------------------- | -------- | ----------- | ---------- | --------- |
| `talos_clusters_list`     | ✔        |             | ✔          | false     |
| `talos_clusters_describe` | ✔        |             | ✔          | true      |
| `talos_clusters_event`    | ✔        |             | ✔          | true      |
| `talos_clusters_members`  | ✔        |             | ✔          | true      |
| `talos_node_logs`         | ✔        |             | ✔          | true      |
| `talos_node_dmesg`        | ✔        |             | ✔          | true      |
| `talos_node_reboot`       |          | ✔           |            | true      |

## 8. Tool Catalog

Every tool that takes `cluster` treats it as optional. When it's empty, the
current context is used.

### 8.1 `talos_clusters_list`

Lists the clusters (talosconfig contexts) the server can access. This tool
makes **no network calls**.

Input: none.

```go
type ClustersListResult struct {
    Current  string           `json:"current" jsonschema:"Default cluster (current talosconfig context)"`
    Count    int              `json:"count" jsonschema:"Number of configured clusters"`
    Clusters []ClusterSummary `json:"clusters" jsonschema:"Configured clusters"`
}

type ClusterSummary struct {
    Name      string   `json:"name" jsonschema:"Cluster name (talosconfig context)"`
    Endpoints []string `json:"endpoints" jsonschema:"Talos API endpoints"`
    Nodes     []string `json:"nodes,omitempty" jsonschema:"Default target nodes"`
    Current   bool     `json:"current" jsonschema:"Whether this is the default cluster"`
    CertExpires string `json:"cert_expires,omitempty" jsonschema:"Client certificate expiry (RFC3339), set when it is expired or expires within 7 days"`
    Discovery   string `json:"discovery,omitempty" jsonschema:"Discovery service endpoint, when configured"`
    Role        string `json:"role" jsonschema:"Credential role: reader or operator (§2.3)"`
    Tools       []string `json:"tools" jsonschema:"Tools usable on this cluster with its credential"`
}
```

`talos_clusters_list` stays offline. It only reports whether discovery is
configured and doesn't contact the discovery service. `Tools` is computed
from the tools that were actually registered: each `Register*` records its
minimum role and whether it needs discovery keys, so `Tools` includes
`talos_clusters_members` only for clusters with discovery keys, and never a
tool that `--extensions` or `--allow-destructive` left out. Node lists
come from `talos_clusters_describe` (apid) and, where configured,
`talos_clusters_members` (discovery).

### 8.2 `talos_clusters_describe`

Gives a cluster-wide health and inventory snapshot. It answers "what is this
cluster and is it healthy?"

```go
type clustersDescribeInput struct {
    Cluster string `json:"cluster,omitempty" jsonschema:"Cluster name (talosconfig context); default is current"`
}

type ClustersDescribeResult struct {
    Cluster           string        `json:"cluster"`
    ClusterName       string        `json:"cluster_name,omitempty" jsonschema:"Cluster name from machine config"`
    KubernetesVersion string        `json:"kubernetes_version,omitempty"`
    Endpoints         []string      `json:"endpoints"`
    NodeSource        string        `json:"node_source" jsonschema:"Where the node list came from: members or talosconfig"`
    Nodes             []NodeSummary `json:"nodes"`
    Etcd              []EtcdMember  `json:"etcd,omitempty" jsonschema:"etcd members (control plane)"`
    Warnings          []string      `json:"warnings,omitempty" jsonschema:"Nodes that could not be queried, unhealthy services, etc."`
}

type NodeSummary struct {
    Hostname        string   `json:"hostname"`
    Addresses       []string `json:"addresses"`
    Role            string   `json:"role" jsonschema:"controlplane or worker"`
    TalosVersion    string   `json:"talos_version"`
    Reachable       bool     `json:"reachable" jsonschema:"Talos API answered for this node"`
    Uptime          string   `json:"uptime,omitempty"`
    Ready           bool     `json:"ready" jsonschema:"Machine status ready"`
    Stage           string   `json:"stage,omitempty" jsonschema:"Machine stage: booting, running, rebooting..."`
    UnhealthyServices []string `json:"unhealthy_services,omitempty"`
    Resources       string   `json:"resources,omitempty" jsonschema:"cpu=N, memory=XGiB (used=YGiB)"`
}

type EtcdMember struct {
    Hostname  string `json:"hostname"`
    ID        string `json:"id"`
    IsLearner bool   `json:"is_learner"`
}
```

Talos API calls:

| Data                       | Source                                                   |
| -------------------------- | -------------------------------------------------------- |
| node list, role, version   | `Pool.Members`: COSI `Members` → talosconfig (§6.1)      |
| version per node           | `client.Version` (fanned out with `client.WithNodes`)    |
| stage / ready              | COSI `MachineStatuses.runtime.talos.dev`                 |
| unhealthy services         | `client.ServiceList` — services not `Running`/healthy    |
| uptime / memory / cpu      | `client.SystemStat` / `client.Memory`                    |
| etcd members               | `client.EtcdMemberList` against one control plane node   |
| k8s version, cluster name  | COSI `KubeletSpecs` / `ClusterIdentity` or machine config cluster name |

Every per-node query goes out in **one** fan-out call using
`client.WithNodes(ctx, nodes...)`, and the response is demultiplexed by
`Metadata.Hostname`. A node that fails adds an entry to `Warnings`. It does
not fail the whole tool, so a partial result is still useful.

`talos_clusters_describe` **does not read or show discovery data**, even when
the context has discovery keys. Everything in the result comes from apid or
the talosconfig. Nodes listed in COSI `Members` that don't answer the fan-out
still appear, with `reachable: false` and only the `Members` data filled in.
If the endpoints themselves are unreachable, the tool fails with the endpoint
error (§11). The agent can then use `talos_clusters_members`, where it is
available, to see the discovery view.

### 8.3 `talos_clusters_event`

Returns recent machined runtime events across the cluster or a single node:
sequence changes, phase/task progress, service state changes, machine status
and config load errors. It is the main tool for "what just happened?" or "is
the reboot done?".

```go
type clustersEventInput struct {
    Cluster string `json:"cluster,omitempty" jsonschema:"Cluster name; default is current"`
    Node    string `json:"node,omitempty" jsonschema:"Limit to one node (IP or hostname); default all nodes"`
    Since   string `json:"since,omitempty" jsonschema:"Only events newer than this duration, e.g. 15m, 2h (default 1h)"`
    Limit   int    `json:"limit,omitempty" jsonschema:"Maximum number of events to return (default 50, max 500)"`
}

type ClustersEventResult struct {
    Cluster string         `json:"cluster"`
    Count   int            `json:"count"`
    Events  []EventSummary `json:"events"`
}

type EventSummary struct {
    Time    string `json:"time" jsonschema:"RFC3339, UTC"`
    Node    string `json:"node"`
    Type    string `json:"type" jsonschema:"sequence, phase, task, service, machine_status, config_load_error, ..."`
    Summary string `json:"summary" jsonschema:"One-line human readable description"`
}
```

Implementation: `client.EventsWatchV2` with `client.WithTailDuration(since)`.
It drains the tail and stops when the backlog is done, or when an idle timeout
expires after the last event (about 1s). The tool never blocks waiting for new
events. Each typed event payload (`machine.SequenceEvent`, `ServiceStateEvent`,
`MachineStatusEvent`, ...) is turned into a one-line `Summary`. Results are
sorted newest first and cut to `Limit`.

### 8.4 `talos_clusters_members`

Lists the cluster's members as the **discovery service** sees them (§2.2).
It answers "which machines have joined this cluster, with what addresses and
roles?" It needs nothing from apid, so it still works when the Talos
endpoints are down, during bootstrap, or when a node's certificates or
network are broken.

The tool exists only where discovery keys exist:

- It is **registered only if at least one context has a valid `discovery`
  block** (`cluster_id` and `cluster_secret`, §2.2). Without one,
  `tools/list` doesn't return it.
- The `cluster` property in its input schema is an `enum` of the clusters
  with discovery, and the description lists them ("Available on: prod-eu").
  `cluster` is optional only when the current context has discovery, and
  then the enum also allows `""` (the current cluster); otherwise it is
  required. `role` allows `""` (all roles) next to the two types.
- A call for a cluster without discovery keys is rejected before any network
  call, with a message listing the clusters that have them. The check lives
  in `Pool.Affiliates` (`ErrNoDiscovery`). There is no fallback to apid.

```go
type clustersMembersInput struct {
    Cluster string `json:"cluster,omitempty" jsonschema:"Cluster name (talosconfig context); default is current"`
    Role    string `json:"role,omitempty" jsonschema:"Filter by machine type: controlplane or worker (default all)"`
}

type ClustersMembersResult struct {
    Cluster   string          `json:"cluster"`
    ClusterID string          `json:"cluster_id" jsonschema:"Discovery cluster ID"`
    Endpoint  string          `json:"endpoint" jsonschema:"Discovery service endpoint queried"`
    Count     int             `json:"count"`
    Members   []MemberSummary `json:"members"`
    Warnings  []string        `json:"warnings,omitempty"`
}

type MemberSummary struct {
    NodeID          string        `json:"node_id" jsonschema:"Affiliate ID (stable node identity)"`
    Hostname        string        `json:"hostname"`
    NodeName        string        `json:"nodename,omitempty" jsonschema:"Kubernetes node name"`
    Role            string        `json:"role" jsonschema:"controlplane or worker"`
    OperatingSystem string        `json:"operating_system,omitempty" jsonschema:"e.g. Talos (v1.11.2)"`
    Addresses       []string      `json:"addresses" jsonschema:"Node addresses reported by the node"`
    Endpoints       []string      `json:"endpoints,omitempty" jsonschema:"KubeSpan WireGuard endpoints (ip:port) stored with the affiliate"`
    APIServerPort   *int          `json:"apiserver_port,omitempty" jsonschema:"kube-apiserver port (control plane only)"`
    KubeSpan        *KubeSpanInfo `json:"kubespan,omitempty" jsonschema:"KubeSpan peer data, when KubeSpan is enabled"`
}

type KubeSpanInfo struct {
    Address             string   `json:"address" jsonschema:"KubeSpan (WireGuard) address"`
    PublicKey           string   `json:"public_key" jsonschema:"WireGuard public key"`
    AdditionalAddresses []string `json:"additional_addresses,omitempty" jsonschema:"Routed prefixes"`
}
```

Implementation:

- The tool calls the discovery `List` RPC for `cluster_id` and decrypts each
  affiliate and its endpoint records with `cluster_secret`, through
  `Pool.Affiliates` (`internal/talos/discovery.go`) and its 30s cache.
- KubeSpan-only affiliates (no `machine_type` or no addresses) are kept and
  shown with `role: ""`, because the point of the tool is to show the raw
  discovery view, which also helps when debugging KubeSpan.
- Members are sorted by role (controlplane first), then hostname, once in
  `Pool.Affiliates`, using the same order as `Pool.Members`.
- Records that fail to decrypt are counted and reported in a single warning,
  for example "3 affiliates could not be decrypted (wrong cluster_secret, or
  another cluster sharing the ID?)".

If the discovery service is unreachable, or no record decrypts, the tool
returns a tool error (`IsError: true`) that says which (§11). It does not
fall back to apid.

Difference from `talos_clusters_describe`: `members` is the discovery
service's view, one cheap call that needs no apid. `describe` is the apid
view (COSI `Members`) plus live health per node, and has no discovery data.
Comparing the two is useful when debugging: a node that appears in
`members` but not in `describe` usually points to a node-side discovery or
apid problem.

### 8.5 `talos_node_logs`

Returns the tail of a Talos service's logs on one node, or of a Kubernetes
container's logs read through containerd.

```go
type nodeLogsInput struct {
    Cluster    string `json:"cluster,omitempty" jsonschema:"Cluster name; default is current"`
    Node       string `json:"node" jsonschema:"Target node (IP or hostname)"`
    Service    string `json:"service" jsonschema:"Service id, e.g. kubelet, etcd, apid, machined, containerd, cri; or container id with kubernetes=true"`
    Kubernetes bool   `json:"kubernetes,omitempty" jsonschema:"Read logs of a Kubernetes container (k8s.io namespace) instead of a Talos service"`
    Tail       int    `json:"tail,omitempty" jsonschema:"Number of lines from the end (default 100, max 1000)"`
    Grep       string `json:"grep,omitempty" jsonschema:"Case-insensitive substring filter applied before tail"`
}

type NodeLogsResult struct {
    Cluster   string   `json:"cluster"`
    Node      string   `json:"node"`
    Service   string   `json:"service"`
    Lines     []string `json:"lines"`
    Count     int      `json:"count"`
    Truncated bool     `json:"truncated" jsonschema:"More lines were available than returned"`
}
```

Implementation: `client.Logs(ctx, namespace, driver, id, follow=false, tailLines)`.
For services, namespace is `system` and driver is `CONTAINERD`. With
`kubernetes=true`, namespace is `k8s.io` and driver is `CRI`. The stream is
read with `client.NewLineReader`.

- Without `grep`, the server sends `tailLines=Tail` and filters nothing.
- With `grep`, it asks for a larger window (`tailLines = 10×Tail`, at most
  10 000 lines), filters, then keeps the last `Tail` matches.

Every line goes through the sanitizer (§10). If the service id is unknown,
the error includes the node's service list from `ServiceList`.

### 8.6 `talos_node_dmesg`

Returns the tail of the kernel ring buffer on one node.

```go
type nodeDmesgInput struct {
    Cluster string `json:"cluster,omitempty" jsonschema:"Cluster name; default is current"`
    Node    string `json:"node" jsonschema:"Target node (IP or hostname)"`
    Tail    int    `json:"tail,omitempty" jsonschema:"Number of lines from the end (default 100, max 1000)"`
    Grep    string `json:"grep,omitempty" jsonschema:"Case-insensitive substring filter applied before tail"`
}

type NodeDmesgResult struct {
    Cluster   string   `json:"cluster"`
    Node      string   `json:"node"`
    Lines     []string `json:"lines"`
    Count     int      `json:"count"`
    Truncated bool     `json:"truncated"`
}
```

Implementation: `client.Dmesg(ctx, follow=false, tail=false)` streams the whole
ring buffer, which is bounded by the kernel buffer size. The server keeps the
last N lines (after `grep`) in a ring. Each line is formatted as
`<RFC3339 time> <facility>.<priority> <message>` and sanitized.

### 8.7 `talos_node_reboot` (destructive)

Reboots a single node and returns once Talos accepts the request. It does not
wait for the node to come back. Agents check progress with
`talos_clusters_event` or `talos_clusters_describe`.

```go
type nodeRebootInput struct {
    Cluster string `json:"cluster" jsonschema:"Cluster name; required, enum of operator clusters (§9)"`
    Node    string `json:"node" jsonschema:"Target node (IP or hostname); exactly one node"`
    Mode    string `json:"mode,omitempty" jsonschema:"default (graceful) or powercycle (skip kexec)"`
}

type NodeRebootResult struct {
    Cluster  string `json:"cluster"`
    Node     string `json:"node"`
    Hostname string `json:"hostname,omitempty"`
    Mode     string `json:"mode"`
    Accepted bool   `json:"accepted"`
    ActorID  string `json:"actor_id,omitempty" jsonschema:"Talos actor id to correlate events"`
    Hint     string `json:"hint" jsonschema:"Next step, e.g. call talos_clusters_event with node=..."`
}
```

Safety rules:

- `node` is **required**. The empty-node fallback from §6.1 is turned off for
  this tool, so a reboot never lands on an implicit target.
- Exactly one node per call. Values containing `,` or several resolved
  addresses are rejected.
- `mode` accepts `default` or `powercycle`. Any other value is rejected.
- Before rebooting a control plane node, the tool checks etcd quorum
  (`EtcdMemberList` plus a health probe). If rebooting the node would leave
  fewer than a majority of healthy members, the tool refuses. The check runs
  only when the node's role is `controlplane`.
- The call is logged at `info` level with cluster, node and mode by the
  logging middleware, the same as in proxmox-mcp.

Implementation: `client.Reboot(client.WithNode(ctx, node), client.WithRebootMode(mode))`.

## 9. Role-based Tool Gating

Tool access is checked at three points. All of them have to allow a call.

1. **Registration** (once, at startup): a tool is registered only if at
   least one cluster's role meets the tool's minimum role (§2.3).
   Destructive tools also need `--allow-destructive`, and
   `talos_clusters_members` needs at least one cluster with discovery keys
   (§8.4). A deployment where
   every cluster has `os:reader` credentials therefore never exposes
   `talos_node_reboot`, even with the flag set. `tools/list` doesn't return
   it, and calling it gives "unknown tool".
2. **Per call**: every handler calls `pool.Require(cluster, minRole)` before
   it makes any Talos call. This matters in mixed setups. For example, with
   `prod` on `os:reader` and `staging` on `os:operator`, the reboot tool is
   registered but `cluster=prod` is rejected right away with "cluster prod
   uses an os:reader credential; talos_node_reboot requires os:operator".
   `talos_clusters_members` checks the same way that the cluster has
   discovery keys.
3. **Talos** enforces the certificate's role in apid. If the server's role
   handling has a bug, the call still can't do more than the certificate
   allows.

The destructive tool tells clients up front where it can be used:

- The tool's `Description` lists the clusters it works on, for example
  "Available on: staging, dev".
- The `cluster` property in its input schema is an `enum` of the operator
  clusters. The schema is built at registration time with a custom
  `InputSchema` instead of being inferred. Because the default context might
  be a reader cluster, `cluster` is **required** for reboot.

`talos_node_reboot` is still registered with `DestructiveHint: true`, so MCP
clients show their own confirmation. With `--context`, there is only one
cluster, so the tool is simply present or absent.

## 10. Output Formatting & Sanitization

- `pkg/formatter.ToText` renders structured results as compact text, the same
  package the sibling projects use. Log and dmesg results render as plain
  lines with a one-line header.
- `internal/utils.Sanitize` is ported from mimiops (`docs/logs.md` there) and
  applied to every log and dmesg line. It masks key-based secrets
  (`token=`, `password=`, `Authorization:`), well-known value formats (JWT,
  PEM blocks, cloud keys) and Talos-specific material such as join tokens,
  `machine.token`, `cluster.secret` and base64 PEM in config dumps. The
  configured `cluster_secret` values are also added to the sanitizer as
  literal strings to mask. Masking is always on.
- Output size is capped: `tail` is clamped to 1000 lines and each line to
  4 KiB (longer lines are cut with an `…` marker). `Truncated` reports whether
  anything was cut.

## 11. Error Handling

- **Input errors** (unknown cluster, unknown or ambiguous node, invalid mode,
  out-of-range `tail`) return a tool error (`IsError: true`) with a message
  that lists the valid choices, such as the known cluster names or member
  hostnames. The goal is that the agent can fix the call without a second
  lookup.
- **gRPC status codes** are mapped to short, actionable messages:
  - `Unavailable` / deadline exceeded: "endpoint X unreachable"
  - `PermissionDenied`: "credential role <role> lacks permission for <api>".
    The per-call role check (§9) should prevent this, so it also logs a
    warning that the §2.3 table doesn't match this Talos version.
  - `Unauthenticated` / expired cert: "client certificate expired at <time>"
- **Partial failures** in fan-out tools appear in `Warnings` and don't fail
  the whole tool (§8.2).
- **Discovery service errors** only affect `talos_clusters_members`, the one
  tool that uses the service. They return a tool error such as "discovery
  service unreachable", "cluster_secret does not decrypt affiliate data
  (wrong secret?)" or "no affiliates registered for cluster_id".
- At startup, client certificate expiry is checked for each context. A
  certificate that is expired or expires within 7 days produces a log
  warning, and a `cert_expires` field is added to `talos_clusters_list`.

## 12. Transports & Deployment

- **stdio (`mcp`)** is meant for local clients. It uses the user's own
  talosconfig.
- **Discovery egress:** a context with `discovery` configured needs outbound
  gRPC/TLS to the discovery endpoint (`discovery.talos.dev:443` by default).
  The Helm chart's optional NetworkPolicy allows that endpoint alongside the
  Talos endpoints.
- **Streamable HTTP (`server`)** serves `/mcp` and `/healthz`, with graceful
  shutdown, the same as in proxmox-mcp. Every caller shares the mounted
  talosconfig credentials. There is no per-request credential passthrough
  because Talos auth is mTLS. In v1, access to the HTTP endpoint has to be
  controlled at the network or ingress level. Per-user authentication through
  a third-party OIDC provider is designed in [`oidc.md`](oidc.md).
- **Helm chart (`charts/talos-mcp`)** mounts talosconfig from a Secret at
  `/etc/talos/config` and sets `TALOSCONFIG`. `allowDestructive: false` is the
  default value.
- **MCPB bundle (`manifest.json`)** runs a binary with `args: ["mcp"]`. A
  `user_config` entry exposes the talosconfig path.
- **Logging middleware** logs every `tools/call` with its name and arguments,
  reusing the proxmox-mcp middleware without the PVE token parsing.

## 13. Out of Scope for v1 / Future

- Omni (`auth.siderov1`) contexts.
- OIDC authentication with per-cluster, per-role grants in HTTP mode
  ([`oidc.md`](oidc.md)).
- Waiting or rolling reboots across several nodes, plus `shutdown`, `upgrade`
  and `reset`.
- More read-only tools: `talos_node_describe` (services, mounts, network
  links/routes, disks), `talos_node_services`, `talos_etcd_status`,
  `talos_node_config` (redacted machine config).
- Hot-reloading talosconfig when the file changes.
- Reading `discovery` settings from a separate file or environment variables,
  so they survive `talosctl config` rewrites (§2.2).

## 14. Testing

- `internal/talos`: table tests for talosconfig validation, context filtering,
  current-context fallback and node resolution, using in-memory
  `clientconfig.Config` fixtures. Also: parsing the `discovery` block, the
  fallback order in `Pool.Members` (COSI `Members` → talosconfig), that
  `Pool.Members` never calls the discovery service, and address selection.
- `internal/talos/discovery`: an in-process fake of the discovery `Cluster`
  gRPC service serving affiliates encrypted with a test secret. Test
  decryption, a wrong secret (error), the 30s cache, and that the client
  never calls `Hello` or `AffiliateUpdate`.
- `talos_clusters_members`: the `role` filter, sort order, KubeSpan fields
  (KubeSpan-only affiliates kept), the warning for records that fail to
  decrypt, the error when the service is unreachable, and rejection of a
  cluster without discovery keys before any network call.
- `talos_clusters_describe`: the result has no discovery data, and the
  discovery fake gets no calls, even when the context has discovery keys.
- `internal/tools`: handler tests against a fake Talos client. The pool hands
  out a small interface (`Version`, `ServiceList`, `Logs`, `Dmesg`,
  `EventsWatchV2`, `Reboot`, `EtcdMemberList`, COSI list) so a fake can stand
  in. Include golden tests for the text output.
- `internal/utils`: sanitizer tests, ported from mimiops and extended with
  Talos secrets.
- `internal/talos`: role parsing from certificate fixtures (reader, operator,
  admin mapped to operator with a warning, several roles, no known role
  skipped).
- `cmd/talos-mcp`: registration matrix. `talos_node_reboot` is absent without
  `--allow-destructive`, absent when every cluster is a reader, and present
  with the operator clusters as its `cluster` enum when the setup is mixed. A
  reboot on a reader cluster is rejected before any Talos call.
  `talos_clusters_members` is absent when no context has discovery keys, and
  present with those clusters as its `cluster` enum otherwise. Every tool
  must have valid input and output schemas.
- Integration (optional, `make test-integration`): `talosctl cluster create`
  (docker provisioner) in CI, then run each tool through `talos-mcp tools`.

## 15. Implementation Plan

The server is built in small steps. Each step compiles, passes `make lint`
and `make unit`, and leaves the binary usable for everything built so far.
Unit tests are plain `_test.go` files with no build tag, so `make unit`
and `go test ./...` run them the same way. Tests for a step land in the
same PR as the code (§14).

### 15.1 Open questions to settle first

These items are marked "to verify" above. Each one changes code in a later
step, so check them against the pinned `pkg/machinery` version before
starting:

1. **Unknown keys in talosconfig (§2.2).** Settled in step 3:
   `clientconfig.FromBytes` ignores the `discovery` key in `pkg/machinery`
   v1.14.2, so nothing is stripped.
2. **Min roles (§2.3).** Already checked against Talos `main`: `Logs`,
   `Dmesg` and the other read APIs allow `os:reader`. Check the same rules
   file again in the pinned release, and add a test that fails when a read
   tool's API is not in the reader set.
3. **Discovery cipher helper (§2.2).** Settled in step 7: the decryption
   is inline in the unexported `parseReply` of `discovery-client` v0.1.15,
   so it is copied with attribution. Only `discovery-api` v0.1.8 is a
   dependency.

### 15.2 Steps

**Step 1 — Module bootstrap.**

- `go mod init github.com/sergelogvinov/talos-mcp`, add the dependencies
  from §5, and pin `pkg/machinery` to the Talos release you target.
- `cmd/talos-mcp/main.go` with the cobra root command, `version` and
  `commit` variables set by the Makefile ldflags, and `version.go`.
- Done when `make build` produces `bin/talos-mcp-<arch>` and
  `talos-mcp version` prints the tag and commit.

**Step 2 — Shared plumbing from the sibling projects.**

- Copy `internal/logger` and `internal/server` (in-memory MCP client) from
  proxmox-mcp, and `pkg/formatter` from the shared package.
- Port `internal/utils.Sanitize` and its tests from mimiops, then add the
  Talos patterns from §10 (join tokens, `machine.token`, `cluster.secret`,
  base64 PEM) and the literal-mask list for `cluster_secret` values.
- Done when the ported tests pass unchanged and the new Talos cases pass.

**Step 3 — Flags and configuration (`internal/config`, `flags.go`).**

- `Flags` with env fallbacks for every row of the §3 table, registered as
  persistent flags on the root command.
- The `Config` struct (§2.4) and talosconfig path resolution: flag, then
  `TALOSCONFIG`, then `~/.talos/config`.
- Read the file and parse it with `clientconfig.FromBytes`, then decode the
  same bytes a second time for the `discovery` block (§2.2).
- Validation from §2.1 and §2.2: at least one context, endpoints and
  `ca`/`crt`/`key` present, Omni contexts skipped with a warning, the
  `--context` filter, and a bad `discovery` block disabling discovery only.
- Done when the table tests from §14 for validation, filtering and the
  current-context fallback pass.

**Step 4 — Roles and certificate checks (`internal/talos/role.go`).**

- Parse each context's `crt` with `crypto/x509`, map the Subject O values
  to `reader` / `operator` (§2.3), warn on `os:admin`, and skip contexts
  with no known role.
- Check certificate expiry and warn when it is expired or within 7 days
  (§11).
- Done when the certificate-fixture tests from §14 pass. Generate the
  fixtures in the test with `crypto/x509`; don't commit real keys.

**Step 5 — Client pool, offline part (`internal/talos/pool.go`).**

- `NewPool`, `List`, `Current`, `Resolve`, `ContextInfo`, `Role`,
  `ClustersWithRole`, `Require` and `Close` from §6. None of these touch
  the network.
- Define the small Talos client interface from §14 (`Version`,
  `ServiceList`, `Logs`, `Dmesg`, `EventsWatchV2`, `Reboot`,
  `EtcdMemberList`, COSI list). `Pool.Client` returns this interface, so
  tool tests can use a fake.
- `Pool.Client`: lazy `client.New(...)`, cached per context, not cached on
  error (§6).

**Step 6 — Tool framework and the first tool.**

- `internal/tools/register.go`, `types.go`, `helpers.go`: `TalosTools`,
  `RegisterTools` with the `--extensions` groups (§7), and helpers for the
  text + structured result, the per-call timeout, and the gRPC error mapping
  from §11.
- `talos_clusters_list` (§8.1). It is offline, so it tests the whole stack
  without a cluster.
- `cmd/talos-mcp/mcp.go` (stdio) and `tools.go` (in-memory client,
  `key=value` arguments, `-o text|json|yaml`).
- Done when `talos-mcp tools talos_clusters_list` works against a real
  talosconfig, and the "every tool has valid input and output schemas" test
  from §14 is in place. Every later tool is covered by that test
  automatically.

**Step 7 — Node resolution and the discovery tool.**

- `Pool.Members` with the apid-only fallback (COSI `Members` →
  talosconfig) and `MemberSource` (§6.1), then node resolution: IP,
  hostname, empty-node default, and address selection. No discovery here.
- `internal/talos/discovery.go`: the read-only `List` client behind
  `Pool.Affiliates`, affiliate decryption (§15.1 item 3), and the 30s cache
  (§2.2). KubeSpan-only affiliates are kept.
- `talos_clusters_members` (§8.4): registered only when
  `ClustersWithDiscovery()` is not empty, with the `cluster` enum, a per-call
  check that the cluster has discovery keys, and no apid fallback.
- Done when the fake discovery gRPC server tests from §14 pass, including
  the test that `Hello` and `AffiliateUpdate` are never called, and the
  registration test shows the tool absent without discovery keys.

**Step 8 — Node read tools.**

- `talos_node_logs` (§8.5) and `talos_node_dmesg` (§8.6): tail and grep
  windows, the ring buffer for dmesg, the 1000-line and 4 KiB caps,
  `Truncated`, and sanitizing every line (§10).
- Both tools need `reader` (§2.3, §15.1 item 2).
- Done when the handler tests against the fake client pass, and the golden
  text output is committed.

**Step 9 — `talos_clusters_event` (§8.3).**

- `EventsWatchV2` with a tail duration, drain until the backlog ends or the
  ~1s idle timeout fires, one-line summaries per event type, newest first,
  cut to `limit`.
- Test the idle-timeout path with a fake stream that never closes.

**Step 10 — `talos_clusters_describe` (§8.2).**

- One `client.WithNodes` fan-out per API from the §8.2 table, demultiplexed
  by `Metadata.Hostname`. Per-node failures go into `Warnings`.
- Merge with `Pool.Members` so nodes listed in COSI `Members` that don't
  answer show `reachable: false`.
- No discovery data in the result, even when the context has discovery
  keys.
- Use the 60s timeout (§6). Test partial failure, the "all endpoints down"
  error, and that the discovery fake gets no calls.

**Step 11 — Destructive tool and role gating (§8.7, §9).**

- `talos_node_reboot`: required `node`, exactly one target, `mode`
  validation, and the etcd quorum check for control plane nodes.
- Registration needs `--allow-destructive` and at least one operator
  cluster. Build the custom `InputSchema` with the `cluster` enum, and list
  the clusters in the description.
- Done when the registration-matrix tests from §14 pass, and a reboot on a
  reader cluster is rejected before any Talos call.

**Step 12 — HTTP transport (`server.go`).**

- Streamable HTTP on `/mcp`, `/healthz`, graceful shutdown on
  SIGINT/SIGTERM, and the `tools/call` logging middleware from proxmox-mcp
  (§12).
- Done when `make run` serves both endpoints and a client can list and call
  tools over HTTP.

**Step 13 — Encrypted secrets ([`secrets.md`](secrets.md)).**

- `internal/secrets` (detect, unlock, decrypt, encrypt), wired into
  `internal/config` during load (secrets.md §5), and the
  `talos-mcp config encrypt|decrypt|check` subcommands in `config.go`.
- This step can run in parallel with steps 7–12. It only touches
  `internal/config` and `cmd/talos-mcp/config.go`.

**Step 14 — Packaging and docs.**

- `charts/talos-mcp`: Secret-mounted talosconfig at `/etc/talos/config`,
  `TALOSCONFIG`, `allowDestructive: false`, and an optional NetworkPolicy
  that allows the Talos endpoints and the discovery endpoint (§12). Add
  `ci/values.yaml` so `make helm-unit` passes.
- `manifest.json` for the MCPB bundle with `args: ["mcp"]` and a
  talosconfig `user_config` entry.
- Check that `Dockerfile` builds with `make images`, then write `README.md`:
  install, credentials (§2.3), client configuration, and the tool list.

**Step 15 — Integration tests (optional).**

- `make test-integration`: create a cluster with `talosctl cluster create`
  (docker provisioner), generate an `os:reader` and an `os:operator`
  talosconfig, and run each tool through `talos-mcp tools`.
- Add it as a separate CI job, so `build-test.yaml` stays fast.

### 15.3 Order and milestones

```
1 → 2 → 3 → 4 → 5 → 6 ──┬─→ 7 → 8 → 9 → 10 → 11 → 12 → 14 → 15
                        └─→ 13 (parallel)
```

- **M1, offline (steps 1–6):** `talos_clusters_list` works over stdio and
  through `tools`. Configuration, roles and registration are tested.
- **M2, read-only (steps 7–10):** every read-only tool works. This is
  enough for a first release with `os:reader` credentials.
- **M3, operator (steps 11–12):** reboot and HTTP mode.
- **M4, release (steps 13–15):** encrypted secrets, Helm chart, MCPB bundle
  and docs.
