# k8s-triage-mcp

A read-only [MCP](https://modelcontextprotocol.io) server that lets an AI agent
diagnose why a Kubernetes workload is unhealthy.

Instead of running six `kubectl` commands and squinting at YAML, you ask *"why
is this deployment unhealthy?"* and the agent gathers the evidence itself.

> **Status: in development.** All four tools are implemented and unit tested. Next: fixtures
> on a local kind cluster and a live demo.

## Design

**Structured data, not prose.** The server supplies facts; the model does the
reasoning. Every tool returns typed JSON with a stable `code` field:

```json
{"code":"probe_too_aggressive","severity":"warning","evidence":{"initialDelaySeconds":5,"observedStartupMs":14200}}
```

not `"Your readiness probe is probably firing too early!"`. Prose makes the
server do the model's job badly, and makes output impossible to compose across
tools.

In Go this is enforced rather than merely intended: `mcp.AddTool[In, Out]`
derives each tool's JSON Schema from its Go result struct by reflection, so a
tool physically cannot return an undeclared shape.

**Read-only.** No scaling, deleting, or patching. An agent that can read a
cluster is useful; one that can mutate it is a liability. This is enforced in
two places, no mutating call exists in the codebase, and
[`deploy/rbac.yaml`](deploy/rbac.yaml) grants only `get`/`list`/`watch`.
ConfigMaps and Secrets are deliberately not granted: diffing env vars reports
that a `valueFrom` reference changed and names the key, but never reads the
value.

## Tools

| Tool | Input | Returns |
|------|-------|---------|
| `get_workload_status` | namespace, name, optional kind | desired vs ready replicas, rollout state, conditions, age |
| `get_pod_events` | namespace, pod or labelSelector, optional window and limit | recent events, merged, newest first, noise filtered |
| `explain_crashloop` | namespace, pod, optional container and tailLines | per container: restarts, last exit code, reason and signal, previous log tail (redacted) |
| `diff_deployment_revisions` | namespace, name, optional fromRevision and toRevision | what changed: image, env, resources, probes, command, volumes |

## Development

```sh
make check   # fmt, vet, test
make build   # binary into bin/
```

Tests run against `client-go`'s fake clientset and need no live cluster.

## License

MIT
