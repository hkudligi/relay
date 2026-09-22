# MCP connections in rly

This note explores how `rly` could work with Model Context Protocol (MCP)
connections when coordinating coding-agent CLIs. It assumes an important
constraint: an agent may already have MCP servers configured, authenticated, and
possibly connected before `rly` starts the task. A connection should therefore
be treated as existing runtime state that can be discovered and governed, not
as something `rly` always creates from scratch.

## Problem statement

`rly` needs to expose controlled inter-agent operations such as `ask_agent`,
`delegate_task`, and `request_review`. At the same time, the coding CLIs it
drives may already have their own MCP configuration and may connect to tools
such as GitHub, databases, issue trackers, or local project services.

Those are two related but different concerns:

1. **Agent communication**: how an agent asks `rly` to coordinate another
   agent.
2. **Tool access**: how an agent accesses an MCP server that provides tools or
   resources.

Conflating them would make ownership, permissions, session reuse, and failure
recovery ambiguous. `rly` should broker agent communication directly and
should govern tool access according to what the adapter and agent actually
support.

## Useful vocabulary

| Concept | Meaning | Durable in `rly`? |
| --- | --- | --- |
| Agent | A configured coding-agent product or executable | Yes |
| Agent session | The resumable upstream conversation/process context | Yes |
| MCP server | A logical provider of tools, resources, or prompts | Configuration and identity only |
| MCP connection | A live transport/client relationship to one server | Usually ephemeral |
| MCP capability | A specific tool, resource, or prompt exposed by a server | Snapshot and trace |
| Connection lease | The bounded permission for a task/session to use a connection | Yes, for audit |
| Connection owner | The component responsible for starting, using, and closing it | Yes |

An MCP connection is not necessarily equivalent to an agent session. One agent
session may use several connections; several sessions may use the same server
through separate connections; and an agent may manage connections internally in
a way that `rly` cannot observe.

## Connection models

### 1. Agent-owned connections

The agent CLI starts and manages all MCP clients. `rly` launches the agent with
its normal configuration and does not proxy tool calls.

```text
rly -> agent session -> MCP client -> server
```

This is the least invasive model and works with existing agent installations.
It is appropriate for an initial adapter when a CLI already supports MCP well.
The trade-off is limited visibility: `rly` may know that an agent supports MCP
without knowing which server was contacted, which tool was called, or whether
the server is trustworthy.

### 2. Broker-owned connections

`rly` owns the MCP client and exposes an approved subset of tools to the agent.
The agent communicates with `rly`, and `rly` makes the MCP call after policy
checks.

```text
rly -> MCP client -> server
  ^
  └── agent session requests an approved tool
```

This gives the strongest auditability and policy enforcement. It also makes
credential handling and cancellation more consistent. It requires adapters to
translate the agent's tool interface to `rly`'s broker and may not be possible
for every CLI.

### 3. Shared or inherited connections

An agent starts with connections that already exist in its environment, or
inherits a connection endpoint, socket, token, or configuration from a parent
process. `rly` records the connection as externally owned and grants a scoped
lease without claiming lifecycle ownership.

This model is useful for a long-running agent, a desktop-hosted agent, or a
user-managed MCP gateway. It is also the most dangerous if “already connected”
is treated as equivalent to “approved.” Discovery must not silently expand the
task's permissions.

### 4. Hybrid model (recommended)

Use the strongest model the adapter supports, while preserving compatibility
with agent-owned connections:

- `rly` always owns its own coordination MCP server for agent-to-agent calls.
- Agent-owned tool connections remain usable only when their declared
  capabilities and policy permit them.
- Sensitive or mutating servers can be routed through a broker-owned gateway.
- Unknown or unverifiable connections are visible but not automatically
  authorized.

This lets the first implementation work with existing CLIs while leaving a
path toward centralized policy and complete audit trails.

## What happens when an agent already has MCP connections?

The adapter should perform a non-invasive connection inventory before starting
or resuming a run. The inventory may come from a CLI capability API, a known
configuration file, process metadata, or an explicit adapter response. It
should never require reading secret values.

For each discovered entry, record only what is necessary:

```json
{
  "server_key": "github",
  "transport": "stdio",
  "source": "agent-config",
  "owner": "agent",
  "status": "declared",
  "capabilities": ["tools"],
  "tool_names": ["search_issues", "create_issue"],
  "mutating": ["create_issue"],
  "credential_ref": "redacted:github-default",
  "verified_at": "2026-09-19T22:10:31Z"
}
```

The important distinction is between states:

- **Declared**: the agent says the server is configured.
- **Reachable**: a bounded health or initialize check succeeded.
- **Authorized**: the current task policy permits use.
- **Leased**: the current run has an explicit time- and scope-bounded grant.
- **In use**: a tool call is currently active.
- **Revoked**: policy changed, the task ended, or a safety condition fired.

An existing connection should be adopted only through an explicit handoff. The
handoff can be automatic for low-risk read-only tools, but mutating tools,
network access, credentials, and cross-repository resources should require a
policy match and, where configured, user approval.

## Proposed lifecycle

```text
discover -> classify -> verify -> authorize -> lease -> use -> revoke/close
```

1. **Discover** connection declarations and adapter capabilities.
2. **Classify** owner, transport, server identity, tool risk, and whether the
   connection is observable by `rly`.
3. **Verify** identity and reachability with a bounded, non-mutating check when
   possible.
4. **Authorize** against user flags, repository policy, task policy, and
   adapter restrictions. Discovery alone never grants access.
5. **Lease** the connection to a task and optionally a session, with allowed
   tools, expiry, and cancellation behavior.
6. **Use** it while recording request metadata, result status, and correlation
   IDs. Redact arguments and results according to secret policy.
7. **Revoke or close** the lease at task completion, cancellation, policy
   change, or connection failure. Only the owner closes the underlying
   connection.

For a pre-existing agent-owned connection, the final step normally revokes the
`rly` lease but does not terminate the agent or delete its configuration.

## Ownership and reuse rules

The connection record should include an ownership mode:

```text
BROKER_OWNED       rly starts and closes the connection
AGENT_OWNED        the agent starts and closes the connection
INHERITED          an external runtime supplied it
SHARED_GATEWAY     a separate MCP gateway owns it for several clients
UNKNOWN            visible for diagnosis, unavailable for automatic use
```

Reuse should be conservative:

- Reuse a broker-owned connection only when its server identity, repository
  scope, credentials, and policy are compatible.
- Do not reuse an agent-owned connection merely because the server name
  matches; verify the session, principal, and allowed tool set.
- Do not share a connection across mutually isolated worktrees unless the
  server itself provides equivalent isolation.
- A resumed agent session may retain its existing connections, but `rly` must
  re-evaluate the lease whenever the task, repository, policy, or credentials
  change.
- After a daemon restart, persisted records describe leases and last-known
  state; they do not prove that a live socket or process still exists.

## API shape for adapters

The existing adapter contract can grow an MCP capability surface without
requiring every adapter to expose internal implementation details:

```text
mcp_capabilities() -> MCPAdapterCapabilities
inspect_connections(session) -> ConnectionInventory
prepare_connection(session, request) -> ConnectionLease | Unsupported
revoke_connection(lease) -> RevokeResult
```

`MCPAdapterCapabilities` should describe whether the adapter supports:

- MCP at all and which transports it accepts.
- Agent-owned connection discovery.
- Passing an `rly` MCP endpoint to a new session.
- Tool allowlists or denylists.
- Per-call cancellation and timeout propagation.
- Structured tool-call events.
- Connection teardown.

An adapter that cannot inspect connections should report `visibility:
unavailable`; it should not claim that no connections exist.

## `rly` coordination MCP server

The coordination server should be a distinct logical server from arbitrary tool
servers. Its tools map to the protocol already defined by the product spec:

```text
ask_agent
delegate_task
request_review
respond_to_request
notify_agent
get_request_status
```

Each tool call carries a task/session identity supplied by the adapter or a
short-lived capability token. The server validates the message envelope,
delegation depth, deadlines, budgets, target eligibility, and policy before
placing a request on the broker. A tool call is a request to the coordinator,
not permission for an agent to launch another process directly.

The coordination server should be offered in two ways:

- **Per-session endpoint** for a newly started agent, minimizing cross-task
  confusion.
- **Daemon endpoint** for agents that can connect to a stable local service,
  with authentication and task binding on every call.

The agent should receive the endpoint through adapter-native configuration or
environment setup, never by embedding credentials in the task prompt.

## Policy and security questions

MCP makes tool access composable, but it does not remove the need for a local
policy boundary. At minimum, `rly` should answer these questions before a
connection is leased:

- Which principal or credential will the server see?
- Is the server local, remote, or reached through a user-managed gateway?
- Is each tool read-only, mutating, destructive, or unknown?
- Is the tool scoped to the current repository and worktree?
- Can calls be cancelled, timed out, and audited?
- Can secrets be redacted from arguments, results, and traces?
- Does the connection survive after the task ends?
- What happens if the agent's configuration is broader than the task policy?

The safe default for an unknown tool is deny. A useful compromise for an
existing connection is to allow declared read-only tools when the server is
trusted and the repository policy permits them, while requiring approval for
mutating tools or unverified servers.

## Persistence and trace

Connections should not be persisted as if they were durable sessions. Persist
identity, ownership, leases, and observations; treat sockets, subprocesses,
and bearer tokens as runtime state.

Suggested records:

```text
mcp_servers       logical server identity and trust classification
mcp_connections   owner, transport, process/session association, health
mcp_capabilities  observed tools/resources/prompts and risk classification
mcp_leases        task/session scope, allowlist, expiry, approval, revocation
mcp_call_events   correlation, timing, outcome, redaction and error metadata
```

Useful trace events include:

```text
mcp.inventory_discovered
mcp.connection_verified
mcp.lease_granted
mcp.tool_call_started
mcp.tool_call_completed
mcp.tool_call_denied
mcp.lease_revoked
mcp.connection_lost
```

Trace payloads should contain server and tool identifiers, not raw credentials
or unrestricted tool arguments. Large results belong in redacted artifacts with
the same retention policy as agent transcripts.

## Failure behavior

| Condition | Expected behavior |
| --- | --- |
| Agent advertises MCP but inventory is unavailable | Continue with declared adapter capabilities; do not assume access |
| Existing connection is unreachable | Mark it unhealthy, release its lease, and offer a bounded retry or fallback |
| Connection is present but policy is narrower | Apply the narrower task policy; do not inherit broader agent permissions |
| Tool list changes after leasing | Re-verify and suspend calls until the lease is renewed |
| Agent-owned connection drops | Let the agent decide whether to reconnect, subject to a new lease |
| Broker-owned connection drops | Reconnect only within retry and approval policy; otherwise fail the call clearly |
| Agent session resumes elsewhere | Require repository, principal, and lease compatibility before reuse |
| Daemon restarts | Reconcile live processes; expire leases that cannot be proven live |
| User cancels the task | Cancel in-flight calls where supported and revoke all task leases |

## Recommended delivery path

### Phase 1: compatibility and coordination

- Add MCP capability metadata to `AgentAdapter`.
- Start `rly`'s coordination MCP server per run or per session.
- Pass the endpoint to adapters that support MCP.
- Treat all other MCP connections as agent-owned and opaque.
- Record declared capabilities and lease decisions without collecting secrets.

### Phase 2: discovery and controlled adoption

- Implement connection inventory for adapters with a stable inspection API.
- Add `mcp_servers`, `mcp_connections`, and `mcp_leases` metadata.
- Permit read-only adoption under repository/task policy.
- Add tool allowlists, cancellation, expiry, and trace events.

### Phase 3: brokered sensitive tools

- Add a broker-owned MCP client for selected servers.
- Route mutating or high-sensitivity tools through that client.
- Add explicit approvals, stronger credential isolation, and replayable call
  metadata.
- Keep agent-owned connections as a compatibility fallback, with their reduced
  observability clearly shown in status and trace output.

## Open decisions

1. Which supported agent CLIs can accept an MCP endpoint or tool configuration
   without modifying user configuration?
2. Should the first coordination server be a Unix-socket MCP server, stdio
   child process, or both?
3. How should an adapter authenticate a stable daemon endpoint without putting
   a reusable token in the agent environment?
4. Which tools qualify as read-only when the server does not declare risk?
5. Should connection leases be task-wide, session-specific, or both?
6. What minimum inventory can each adapter provide without exposing credentials
   or provider-internal state?

## Recommendation

Adopt the hybrid model. Make `rly`'s coordination MCP server a first-class,
broker-owned capability, because inter-agent requests must pass through the
control plane. At the same time, preserve existing agent MCP connections as
agent-owned runtime state: discover them where possible, classify their risk,
and require a scoped lease before treating them as available to a task.

This approach respects existing agent setups, avoids pretending that opaque
connections are fully controlled, and creates a gradual path from compatibility
to auditable tool brokering.
