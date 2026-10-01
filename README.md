# Talos opinionated MCP Server

> **NOTE:**
> This project is under active development.

## Motivation

Modern infrastructure often uses tools such as Terraform/OpenTofu, Ansible,
and GitOps. These tools deploy and configure Talos Linux nodes and Kubernetes
clusters.

The Talos MCP Server does not replace these tools. It gives AI assistants
and automation agents access to information about your Talos clusters.

The default tools are read-only. An optional action tool can be enabled with
`--allow-destructive` to reboot a node.

Instead of running many `talosctl` commands across nodes, you can ask an AI
assistant to collect and analyze the required information. Your existing
automation tools remain the main source for infrastructure changes.

## Overview

The server connects an MCP client, such as an AI assistant, to the Talos API
(apid) and, optionally, to the Talos discovery service. It supports multiple
Talos clusters and returns structured data.

You can use it to:

- inspect clusters, nodes, and their Talos and Kubernetes versions
- read Talos service logs and kernel logs (dmesg) of a node
- review recent Talos runtime events
- list cluster members from the discovery service, even when the Talos API is down
- collect information before you make infrastructure changes

**Keep your AI on the leash.**

## Talos tools

The MCP server provides the following tools:

| Tool | Arguments | Description |
| --- | --- | --- |
| `talos_clusters_list` | None | List the configured clusters with their endpoints, credential role, and the tools usable on each. Makes no network calls. |
| `talos_clusters_describe` | `cluster` (optional) | Show every node of a cluster with its role and Talos and kubelet versions. |
| `talos_clusters_event` | `cluster`, `node`, `since`, `limit`, `actor_id` (all optional) | List recent Talos runtime events, newest first. Defaults: last `1h`, `50` events. |
| `talos_clusters_members` | `cluster`, `role` (optional) | List cluster members as the discovery service sees them: node ID, hostname, role, addresses, and KubeSpan data. Available only on clusters with discovery keys. |
| `talos_node_logs` | `service`, `cluster`, `node`, `kubernetes`, `tail`, `grep` (optional except `service`) | Show the last lines of a Talos service's logs (`kubelet`, `etcd`, `apid`, `machined`, ...) or of a Kubernetes container. |
| `talos_node_dmesg` | `cluster`, `node`, `tail`, `grep` (all optional) | Show the last lines of a node's kernel log. Secrets in the output are masked. |

`cluster` is a talosconfig context name. When it is omitted, the current
context is used. Use `talos_clusters_list` first to find the cluster names.
MCP clients can discover the full input and output schemas. You can also list
the tools from the command line:

```sh
talos-mcp tools --talosconfig /absolute/path/to/talosconfig
```

When `--allow-destructive` is enabled and the cluster's credential has the
`os:operator` role, `talos_node_reboot` accepts `cluster`, `node`, and an
optional `mode` (`default` or `powercycle`). It returns once Talos accepts the
request; it does not wait for the node to come back.

The tools available on each cluster depend on the role of its client
certificate: `os:reader` gets the read-only tools, `os:operator` also gets
the reboot tool.

## Installation

For a local installation with Homebrew, run:

```sh
brew install sergelogvinov/tap/talos-mcp
```

You do not need a local installation when you use an MCP server hosted on
another machine.

## Configure Talos clusters

The server reads a standard **talosconfig**, the same file `talosctl` uses.
Each context is one cluster. Create a dedicated credential with the smallest
role you need:

```sh
talosctl config new --roles os:reader --crt-ttl 8760h ~/.talos/mcp-config
```

A context may also have an optional `discovery` block with the cluster ID and
secret, which enables `talos_clusters_members`:

```yaml
context: prod-eu
contexts:
  prod-eu:
    endpoints: [10.0.0.10, 10.0.0.11, 10.0.0.12]
    ca: LS0t...
    crt: LS0t...
    key: LS0t...
    discovery:
      cluster_id: 3x9y...
      cluster_secret: c2VjcmV0...
```

The private `key` and `cluster_secret` can be stored encrypted with a
passphrase, an age key, or your SSH key:

```sh
talos-mcp config encrypt --talosconfig ~/.talos/mcp-config -o ~/.talos/mcp-config.enc \
    --recipient-file ~/.ssh/id_ed25519.pub
talos-mcp config check --talosconfig ~/.talos/mcp-config.enc \
    --talosconfig-identity ~/.ssh/id_ed25519
```

Set the talosconfig path with `--talosconfig` or `TALOSCONFIG`. The default is
`~/.talos/config`.

See [docs/config.md](docs/config.md) for every talosconfig field, the
discovery block, credential roles, the unlock options for encrypted secrets,
the `config encrypt`, `decrypt`, and `check` commands, deployment recipes, and
troubleshooting.

## Configure an MCP client

### Local stdio server

For clients that use a JSON MCP configuration, add an entry like this:

```json
{
  "mcpServers": {
    "talos": {
      "command": "talos-mcp",
      "args": ["mcp"],
      "env": {
        "TALOSCONFIG": "/absolute/path/to/talosconfig"
      }
    }
  }
}
```

If the talosconfig has encrypted fields, also set an unlock source, for
example `"TALOSCONFIG_IDENTITY": "/absolute/path/to/.ssh/id_ed25519"`. The
`mcp` command never prompts for a passphrase.

### Remote HTTP server

Start the streamable HTTP server with:

```sh
talos-mcp server \
  --talosconfig /absolute/path/to/talosconfig \
  --listen 0.0.0.0 \
  --port 8080
```

The MCP endpoint is `http://host:8080/mcp`, and a health check is served on
`/healthz`.

For a remote client, use an HTTPS URL that ends with `/mcp`:

```json
{
  "mcpServers": {
    "talos": {
      "type": "http",
      "url": "https://talos-mcp.example.com/mcp"
    }
  }
}
```

The server does not provide user authentication for the HTTP endpoint. Every
caller uses the credentials from the server's talosconfig, so protect the
endpoint with a network policy or an authenticating proxy.

A Helm chart for Kubernetes is available in [charts/talos-mcp](charts/talos-mcp).

Restart or reload the MCP client after you save its configuration.

## Running

### Common flags

The `mcp`, `server`, and `tools` commands use the following flags. Each flag
can also be set with an environment variable. A command-line flag has higher
priority than an environment variable.

| Flag | Environment variable | Default | Description |
| --- | --- | --- | --- |
| `--talosconfig <path>` | `TALOSCONFIG` | `~/.talos/config` | Path to the talosconfig file. |
| `--context <name>` | `TALOS_CONTEXT` | All contexts | Use only this one talosconfig context. |
| `--talosconfig-identity <path>` | `TALOSCONFIG_IDENTITY` | None | age identity or OpenSSH private key that decrypts encrypted fields. Repeatable. |
| `--talosconfig-passphrase-file <path>` | `TALOSCONFIG_PASSPHRASE_FILE` | None | File with the passphrase of encrypted fields. |
| `--talosconfig-askpass <program>` | `TALOSCONFIG_ASKPASS` | None | Program that prints the passphrase of encrypted fields. |
| `--extensions <list>` | `EXTENSIONS` | `all` | Comma-separated tool groups to enable: `cluster`, `node`, or `all`. |
| `--allow-destructive` | `ALLOW_DESTRUCTIVE` | `false` | Allow destructive tools, such as `talos_node_reboot`. |
| `--log-level <level>` | `LOG_LEVEL` | `info` | Log level: `debug`, `info`, `warn`, or `error`. |
| `--log-format <format>` | `LOG_FORMAT` | `text` | Log output format: `text` or `json`. |

The `server` command also accepts `--port` (`PORT`, default `8080`),
`--listen` (`LISTEN`, default `127.0.0.1`), and `--require-all-contexts`
(`REQUIRE_ALL_CONTEXTS`). The `tools` command accepts `--output` (`-o`) with
`text`, `json`, or `yaml`. Its default is `text`.

For example, run the stdio server with JSON logs:

```sh
talos-mcp mcp \
  --talosconfig /absolute/path/to/talosconfig \
  --log-format json
```

## Test the configuration

Check the talosconfig and its encrypted fields:

```sh
talos-mcp config check --talosconfig /absolute/path/to/talosconfig
```

List all available tools:

```sh
export TALOSCONFIG="/absolute/path/to/talosconfig"
talos-mcp tools
```

Call a tool directly:

```sh
talos-mcp tools talos_clusters_list
talos-mcp tools talos_clusters_describe cluster=prod-eu
talos-mcp tools -o json talos_node_logs cluster=prod-eu node=10.0.0.10 service=kubelet tail=50
```

## License

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

[http://www.apache.org/licenses/LICENSE-2.0](http://www.apache.org/licenses/LICENSE-2.0)

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

---

`Talos Linux` is a trademark of [Sidero Labs, Inc.](https://www.siderolabs.com)
