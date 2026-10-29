# k8s-triage-mcp

[![ci](https://github.com/whoismansi/k8s-triage-mcp/actions/workflows/ci.yml/badge.svg)](https://github.com/whoismansi/k8s-triage-mcp/actions/workflows/ci.yml)

A read-only [MCP](https://modelcontextprotocol.io) server that gives an AI agent
the cluster state you would otherwise collect by hand when a Kubernetes workload
is unhealthy: rollout status, container exit state, previous logs, events, and
what changed between deployment revisions.

You ask "why is `orders` broken?" and the agent gathers the evidence and works
out the cause, instead of you running `kubectl describe`, `kubectl logs
--previous`, `kubectl get events` and `kubectl rollout history` and reading
through the output.

## Why tools instead of kubectl

You can give an agent a shell and let it run `kubectl`. It mostly works, but it
has two problems.

First, the raw output is long and the useful part is easy to misread. A
container that exits with code 137 was killed with SIGKILL. That could be the
OOM killer or a failed liveness probe; only the kubelet's `reason` field tells
you which. `kubectl describe` shows both, among sixty other lines, and an agent
skimming it will often say "out of memory" either way.

Second, a shell with cluster credentials can do anything those credentials
allow, including deleting things.

These tools read the same API objects and return structured findings instead of
text. Each finding has a stable `code` and the raw values behind it:

```json
{
  "code": "killed_by_signal",
  "severity": "warning",
  "message": "container was killed by a signal",
  "evidence": {
    "container": "app",
    "exitCode": 137,
    "signalName": "SIGKILL",
    "reason": "Error",
    "hasLivenessProbe": true,
    "runSeconds": 9
  }
}
```

The tool does not claim this is a liveness failure. It reports that the process
was killed, that the kubelet did not report an OOM, and that a liveness probe is
configured, and leaves the conclusion to the agent. `oom_killed` is only
reported when the kubelet's reason is `OOMKilled`.

## Tools

| Tool | Answers | Inputs |
|---|---|---|
| `get_workload_status` | Are replicas missing? Did the rollout stall? | namespace, name, optional kind |
| `explain_crashloop` | Why does this container keep dying? | namespace, pod, optional container, tail lines |
| `get_pod_events` | What has the cluster been reporting about these pods? | namespace, pod or label selector, optional window, limit |
| `diff_deployment_revisions` | What changed since it last worked? | namespace, deployment, optional from and to revisions |

A few details that matter in practice:

- `get_workload_status` handles Deployments, StatefulSets and DaemonSets, and
  detects the kind if you don't pass one. It won't report a workload as healthy
  while the controller hasn't observed the latest spec, or while old pods are
  still serving because the new revision can't start.
- `explain_crashloop` reports every container, including init containers, and
  reads the log of the instance that actually crashed. Log tails default to 50
  lines and are capped at 200 lines and 64 KiB.
- `get_pod_events` reads both the core and `events.k8s.io` APIs, merges
  repeats, hides routine events like `Scheduled` and `Pulled`, and tells you how
  many it hid and what time window it searched.
- `diff_deployment_revisions` defaults to the latest revision against the one
  before it. It compares image, env, resources, probes, command and args, and
  volumes, and names any other changed field without interpreting it.

### Finding codes

Agents key on these, so they are treated as a stable API. New codes may be
added; existing ones are not renamed.

| Area | Codes |
|---|---|
| Rollout | `replicas_unavailable`, `rollout_incomplete`, `rollout_stuck`, `progress_deadline_exceeded` |
| Container exit | `crashloop_backoff`, `oom_killed`, `killed_by_signal`, `command_not_found`, `segfault`, `non_zero_exit`, `exited_cleanly`, `container_start_failed` |
| Startup | `image_pull_failed`, `container_config_error`, `pod_create_failed`, `pod_not_started`, `unschedulable`, `volume_mount_failed`, `evicted`, `probe_failing` |
| Revision diff | `image_changed`, `env_changed`, `resources_changed`, `probe_changed`, `command_changed`, `other_spec_changed` |

## Install

Requires Go 1.26 or later.

```sh
go install github.com/whoismansi/k8s-triage-mcp/cmd/k8s-triage-mcp@latest
```

Or build from source:

```sh
git clone https://github.com/whoismansi/k8s-triage-mcp
cd k8s-triage-mcp
make build   # writes bin/k8s-triage-mcp
```

The server reads your kubeconfig the same way `kubectl` does: `--kubeconfig`
if given, then `$KUBECONFIG`, then `~/.kube/config`.

## Connect it to an agent

### Claude Code

```sh
claude mcp add k8s-triage -- /path/to/k8s-triage-mcp --context my-cluster
```

Everything after `--` is the command Claude Code runs, so `--context` is a flag
for this server, not for Claude Code. Pin it. Without it the server follows
whatever your current kubeconfig context is, which changes every time you run
`kubectl config use-context` or create a kind cluster.

### Claude Desktop

Add this to `claude_desktop_config.json`:

```json
{
  "mcpServers": {
    "k8s-triage": {
      "command": "/path/to/k8s-triage-mcp",
      "args": ["--context", "my-cluster"]
    }
  }
}
```

Any other MCP client that can launch a stdio server works the same way.

## Read-only, and what that does and doesn't cover

The code makes no create, update, patch or delete calls. That is enforced by
code review, not by the type system, so don't rely on it alone.

The server runs with whatever credentials your kubeconfig context has. If that
context is cluster-admin, the server could in principle do anything an admin
can. For anything other than a local test cluster, run it with a dedicated
service account bound to the ClusterRole in [`deploy/rbac.yaml`](deploy/rbac.yaml),
which grants only `get` and `list` on the resources the tools read. Every tool
call has a 30 second deadline, and an empty namespace is rejected rather than
treated as "all namespaces".

That role deliberately excludes Secrets and ConfigMaps. When an env var comes
from a Secret, the diff tool reports the Secret name and key that changed, never
the value.

Logs are the gap RBAC can't close: applications print connection strings and
tokens at startup, and those go wherever the tool output goes, including to the
model provider. The same text also turns up outside logs, because a container's
termination message, its waiting reason, pod conditions and events can all quote
what the process printed. Every one of those fields is redacted, not just the
log lines. The redaction covers common credential shapes (passwords in URLs,
bearer and basic auth, JWTs, cloud and GitHub-style keys, `--password` style
flags, values of variables named like `PASSWORD` or `API_KEY`), and
`explain_crashloop` reports how many it replaced. This is a
best-effort pattern match. A count of zero means nothing matched a known
pattern, not that the log is clean.

## Try it on a local cluster

`fixtures/` contains deployments that are broken on purpose, one per failure
mode. The demo script creates a [kind](https://kind.sigs.k8s.io) cluster and
applies all of them.

Requires Docker, kind and kubectl.

```sh
make kind-up       # create the k8s-triage-demo cluster and apply the fixtures
make kind-wait     # wait until each one has actually failed
make demo SCENARIO=liveness-kill
make kind-down     # delete the cluster
```

Creating the cluster switches your current kubectl context to
`kind-k8s-triage-demo`.

| Scenario | What's broken | What the tools report |
|---|---|---|
| `bad-command` | runs a binary that isn't in the image | `command_not_found`; the log shows `run-worker: not found` |
| `memory-hog` | exceeds a 32Mi memory limit | `oom_killed` with the limit |
| `liveness-kill` | liveness probe always fails; the process ignores SIGTERM | `killed_by_signal`, exit 137, not `oom_killed` |
| `bad-image` | image tag doesn't exist | `image_pull_failed`, `progress_deadline_exceeded` |
| `too-big` | requests 999 CPUs | `unschedulable`; `pod_not_started` from the crashloop tool |
| `leaky` | prints a database password and exits | the password comes back as `[REDACTED]` |
| `orders` | the second revision renames a ConfigMap key | see below |

### Example: "it worked before the last deploy"

`orders` deploys a healthy first revision, then a second one that changes the
image tag and renames the ConfigMap key the app reads `DB_HOST` from. The old
pods keep serving, so `kubectl get deploy` shows `READY 2/2`.

`get_workload_status` doesn't take that at face value:

```text
rollout_incomplete          desired 2, updated 1, ready 2
progress_deadline_exceeded
```

`diff_deployment_revisions` shows two changes between revisions 1 and 2:

```json
[
  {"category": "image", "field": "image", "change": "modified",
   "from": "nginx:1.27", "to": "nginx:1.27-alpine"},
  {"category": "env", "field": "env.DB_HOST", "change": "modified",
   "from": "configMapKeyRef(app-config/db_host)",
   "to": "configMapKeyRef(app-config/database_host)"}
]
```

And `explain_crashloop` on the new pod says which one matters:

```text
container_config_error  couldn't find key database_host in ConfigMap triage-demo/app-config
```

The image change is a red herring. Each tool only reports what it can see, and
the cause comes from putting the three together.

## CLI

Every tool is also available as a subcommand that prints the same JSON the
agent receives. Useful for scripting, and for checking whether a bad diagnosis
came from the data or from the agent's reasoning.

```sh
k8s-triage-mcp status    -n triage-demo orders
k8s-triage-mcp crashloop -n triage-demo <pod> [-container NAME] [-tail N]
k8s-triage-mcp events    -n triage-demo -l app=orders [-since MIN] [-limit N] [-all]
k8s-triage-mcp diff      -n triage-demo orders [-from REV] [-to REV]
```

All subcommands accept `--kubeconfig` and `--context`. With no subcommand, the
binary runs the MCP server on stdio.

## Development

```sh
make check   # gofmt, go mod tidy, vet, golangci-lint, tests with -race
make build
```

`make check` runs the same steps as CI. The linter needs golangci-lint v2
(`brew install golangci-lint`).

Unit tests use client-go's fake clientset and need no cluster. That covers the
logic but not how a real kubelet reports state. The first run against the kind
fixtures found three bugs the unit tests had passed, each now covered by a test
that reproduces the real state. If you change how a tool reads container status
or events, run the fixtures.

The code is split so the diagnostics don't depend on MCP:

```text
cmd/k8s-triage-mcp   flags, CLI subcommands, stdio wiring
internal/triage      all diagnostic logic; takes a kubernetes.Interface, returns structs
internal/mcpserver   registers each triage function as an MCP tool
internal/kube        kubeconfig loading
```

## Limitations

- Revision diffs are for Deployments only. StatefulSet and DaemonSet history is
  stored differently and isn't supported.
- Events are kept by the API server for about an hour by default, so older
  failures may have no events left to report.

## License

MIT. See [LICENSE](LICENSE).
