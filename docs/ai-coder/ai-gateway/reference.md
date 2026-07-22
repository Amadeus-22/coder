# Reference

> [!NOTE]
> AI Gateway requires the [AI Governance Add-On](../ai-governance.md).
> As of Coder v2.32, deployments without the add-on cannot access AI Gateway.

## Deployment topologies

AI Gateway can run inside `coderd` or as a standalone data-plane service.
Both topologies use the same Gateway request handling and keep `coderd` as the source of truth for user and credential validation, budget checks, provider configuration, MCP configuration, and AI session records.

### Embedded Gateway

By default, `coder server` runs an in-memory Gateway instance in the `coderd` process.
AI clients send requests to the Coder access URL, and the embedded Gateway handles AI traffic without a network control connection between components.

The following diagram shows the embedded topology:

![AI Gateway implementation details](../../images/aibridge/aibridge-implementation-details.png)

### Standalone Gateway

A [standalone deployment](./standalone.md) runs the AI traffic data plane outside the `coderd` process.
Each replica accepts client traffic, sends AI requests directly to upstream providers, and maintains a control connection to `coderd` using an AI Gateway key.
The control connection carries user and credential validation, budget checks, provider and MCP configuration, provider change notifications, and AI session records.

Standalone replicas do not own authoritative database state.
They keep ephemeral provider snapshots, request caches, provider key pools, and metrics in memory, and emit their own logs and traces.
If you enable `CODER_AI_GATEWAY_DUMP_DIR`, each replica also writes request and response dumps to local disk.
Protect these files as sensitive data and use persistent storage if you need them to survive pod replacement.
You can place multiple replicas behind a load balancer and restart, scale, or upgrade the data plane independently from `coderd`.
Sticky load balancing can improve cache efficiency but is not required for correctness.

`coderd` remains required for standalone operation.
A replica becomes unready when its control connection is unavailable, even if its HTTP listener remains healthy.
AI Gateway Proxy remains part of `coder server` and can forward its intercepted traffic to either the embedded Gateway or a standalone endpoint.

## Standalone protocol compatibility

Standalone replicas and `coderd` communicate through an internal protocol.
The current protocol version is `1.2`.
`coderd` validates the protocol version advertised by each standalone replica before it accepts the control connection.

Compatibility follows these rules:

- The Gateway and `coderd` protocol major versions must match.
- The Gateway protocol minor version must be less than or equal to the `coderd` protocol minor version.
- An older `coderd` rejects a standalone Gateway that advertises a newer protocol minor version.

The Coder build versions are logged with the protocol versions for observability, but build version equality is not the compatibility criterion.
Use matching Coder releases for `coderd` and standalone Gateway replicas instead of relying on version skew.

When upgrading both components, upgrade `coderd` first and then roll out the matching standalone Gateway release.
When rolling back both components, roll back the standalone Gateway first and then roll back `coderd`.
This order prevents a newer Gateway protocol from connecting to an older control plane that cannot support it.

## Supported APIs

API support is divided into two categories:

- **Intercepted**: Requests are intercepted, audited, and augmented with full AI Gateway functionality.
- **Passthrough**: Requests are proxied directly to the upstream provider without auditing or augmentation.

Where relevant, both streaming and non-streaming requests are supported.

### OpenAI

#### Intercepted

- [`/v1/chat/completions`](https://platform.openai.com/docs/api-reference/chat/create)
- [`/v1/responses`](https://platform.openai.com/docs/api-reference/responses/create)

#### Passthrough

- [`/v1/models(/*)`](https://platform.openai.com/docs/api-reference/models/list)

### Anthropic

#### Intercepted

- [`/v1/messages`](https://docs.claude.com/en/api/messages)

#### Passthrough

- [`/v1/models(/*)`](https://docs.claude.com/en/api/models-list)

## Troubleshooting

To report a bug, file a feature request, or review known issues, visit the [Coder GitHub repository](https://github.com/coder/coder/issues).
For help with AI Gateway, visit the [Coder Discord](https://discord.gg/coder).
