# Matchmaker Threat Model

Status: Draft
Date: 2026-08-28 (updated after source review of charmbracelet/crush at v0.91.2)

Method: [STRIDE](https://learn.microsoft.com/en-us/previous-versions/commerce-server/ee823878(v=cs.20)), selected from the method catalog of the
[OWASP Threat Modelling Guide](https://owasp.org/www-project-threat-modelling-guide/).
STRIDE fits this system because Matchmaker is a process/network orchestrator
whose risks concentrate on spoofed identities, tampered state, repudiated
actions, leaked secrets, resource exhaustion, and privilege escalation across
component boundaries.

This document defines what Matchmaker's threat model covers, what it delegates
to Crush, and where the trust boundaries lie. It refines (and does not
replace) the security model in [DESIGN.md §6](DESIGN.md). §4.2 and the Appendix
are grounded in a source review of Crush v0.91.2 (the version pinned by DESIGN
§9.3); re-verify when the pin moves.

## 1. System description

Matchmaker is a single-operator, single-host orchestrator that coordinates a
fleet of `crush server` processes (one per project, loopback only) to execute
goal DAGs. See [DESIGN.md](DESIGN.md) §2–5 for the full architecture. Security
relevant facts:

- All Crush servers bind to `127.0.0.1` with **no authentication** (C2) —
  confirmed at source level in v0.91.2: no auth middleware, tokens, TLS, or
  origin checks on any endpoint (`internal/server/server.go`).
- Matchmaker spawns servers as child processes via `os/exec`.
- Matchmaker persists all goals, steps, runs, and notes in a local SQLite
  store **before dispatch**.
- Agents (LLM-driven, executing arbitrary tools inside project workspaces)
  exchange notes through a multiplexed MCP server hosted by Matchmaker on
  loopback, unauthenticated.
- Crush API responses embed provider API keys in plaintext (confirmed:
  `GET /v1/config`, `GET /v1/workspaces`, and `GET /v1/workspaces/{id}/config`
  all serialize resolved keys with no redaction); Matchmaker retains only
  required typed fields and must not log or persist raw API payloads (DESIGN §6,
  §9.6). Crush remains the system of record for session messages and tool
  history; Matchmaker stores references rather than copies.
- The Crush API includes endpoints far more powerful than Matchmaker needs,
  including an **unauthenticated remote shell**
  (`POST /v1/workspaces/{id}/agent/sessions/{sid}/shell`, no permission gate,
  v0.91.2 `internal/backend/agent.go`) and remote yolo
  (`POST .../permissions/skip`). See the Appendix.

## 2. Actors and assets

### 2.1 Actors

| Actor | Description |
|---|---|
| **Operator** | The single trusted local user who submits goals and owns the fleet config and store. |
| **Agent** | An LLM-driven Crush session executing inside a project workspace. Semi-trusted: it runs arbitrary commands (subject to permission policy) and is susceptible to prompt injection from repository content and from notes. |
| **Co-located local process** | Any other process running as the same (or a privileged) user on the host. Untrusted: can connect to any loopback port. |
| **Upstream content** | Repository files, fetched web content, issue text, note bodies, and **project config files** (`crushrc` plus legacy JSON while Crush supports it) that enter an agent's context or Crush's loader. Untrusted and **attacker-adjacent**: an external attacker who can influence repo or note content can attempt indirect prompt injection, and repo-committed `crushrc` is executable shell run by Crush at workspace creation (Appendix A2). |
| **LLM provider** | Remote API endpoint reached only by Crush servers. Outside the host trust domain. |

### 2.2 Assets

| Asset | Security property at stake |
|---|---|
| Fleet config (paths, ports, tags) | Integrity, availability |
| SQLite store (goals, steps, runs, notes, operational history) | Integrity, confidentiality, availability; no tamper-resistance guarantee |
| Provider API keys (in Crush configs and API responses) | Confidentiality |
| Managed project working trees (source code) | Integrity |
| Host execution environment | Integrity, availability (agents execute arbitrary commands) |
| Cooperative note channel | Availability and best-effort attribution; note content is non-confidential and untrusted |
| Operator intent records | Operational reconstruction only; no non-repudiation guarantee |

## 3. Trust boundaries

| ID | Boundary | Crosses between | Data crossing | Controls |
|---|---|---|---|---|
| **TB1** | Operator ↔ Matchmaker | Trusted human → orchestrator process | Goal DAGs, fleet config, project-config fingerprint approvals | Local file permissions on config and store; OS user identity. |
| **TB2** | Matchmaker ↔ Crush servers | Orchestrator → unauthenticated loopback HTTP/SSE | Workspace creation, prompts, permission grants, cancels; events, run results, **secret-bearing config responses** | Loopback binding only (DESIGN §6); Matchmaker is the only intended client; retain only required typed response fields before log/store (DESIGN §6). |
| **TB3** | Matchmaker ↔ OS (process spawn) | Orchestrator → child processes, filesystem | `os/exec` argv (host, typed options), workspace paths, signals, `.crushrc` owned-block edits | Port/path/data-dir collision validation; no shell invocation for process spawn; bounded owned-block config writes. |
| **TB4** | Agents ↔ Coordination MCP server | Semi-trusted agents → unauthenticated loopback MCP endpoint | Project-wide `note_send(goal, from, to, body)`, `note_read(goal, for, since)`, `result_read(goal, run, cursor)`; note bodies and run results | Claimed project fields are unauthenticated. Registration is optional per project but cannot enforce per-step access. Cooperative, non-confidential channel; goal checks prevent accidental misuse only; bounded reads plus size and rate limits (§5.5). |
| **TB5** | Agent ↔ project working tree | Semi-trusted agent → source files | Arbitrary reads/writes/commands within the workspace | Crush permission system, supervised by Matchmaker (deny default); out of Matchmaker's direct control (§4.2). |
| **TB6** | Crush servers ↔ LLM providers | Host → remote API | Prompts, tool results, code content; provider keys outbound | Owned entirely by Crush (§4.2). |
| **TB7** | Matchmaker ↔ future remote API | Orchestrator → network listeners | Goal submission, reports | Not in current scope; when added (DESIGN §8), authentication is mandatory at that layer (DESIGN §6). |

```mermaid
flowchart TB
    subgraph HOST["Host trust domain (single OS user)"]
        OP(["Operator"]) -->|TB1: goals, config| MM
        subgraph MM["Matchmaker"]
            STORE[("SQLite store")]
        end
        MM <-->|TB2: HTTP/SSE, no auth, loopback| S1["crush server A"]
        MM <-->|TB2| S2["crush server B"]
        MM -->|TB3: os/exec, signals, .crushrc merge| S1
        S1 <-->|TB4: coordination MCP, no auth, loopback| MM
        S2 <-->|TB4| MM
        S1 <-->|TB5: workspace files| REPOA["/repo/a"]
        S2 <-->|TB5| REPOB["/repo/b"]
        LOCAL(["Other local processes"]) -.->|"loopback ports (unauthenticated)"| S1
        LOCAL -.-> MM
    end
    S1 -->|TB6: prompts + keys| LLM(["LLM provider"])
    S2 -->|TB6| LLM
    EXT(["Upstream content (repos, web, notes)"]) -.->|prompt injection surface| S1
```

## 4. Scope

### 4.1 In scope (Matchmaker owns)

Threats against components Matchmaker implements or configures:

1. **Fleet and project config handling** — fleet parsing; port, canonical-path,
   and data-directory collision validation; typed server options; authoritative
   host/workspace path; operator-approved installation of a deterministic owned
   `crushrc` block; and deployment fingerprints covering applicable project and
   global config, selected environment, binary, and data directory before
   lifecycle operations (Appendix A2). Matchmaker never depends on legacy JSON.
   Approval does not validate operator-supplied executable config.
2. **Store** — SQLite integrity, corruption handling, file permissions, and
   injection via stored notes and orchestration metadata read into prompts or
   templates. Crush session messages and tool history are referenced, not
   duplicated.
3. **Goal validation, dispatch, and templating** — atomic submission validation,
   bounded DAG and target expansion, prompt template rendering (`text/template`)
   with upstream excerpts and note injection, and per-instance serialization.
4. **Supervision** — v1 permission policy enforcement (`deny`/`grant_all`),
   rejection of the reserved future `ask` policy, timeouts, cancellation, and
   SSE stream handling. Note: Crush permission grants
   are **in-memory per server process** (v0.91.2 `internal/permission`); a
   server restart resets them, which Matchmaker's retry path must account for.
5. **Coordination MCP server** — optional project-wide registration, monotonic
   note IDs, durable per-audience delivery cursors, frozen addressing, bounded
   prompt injection, rate/size/count limits, and bounded retrieval of completed
   run results by goal/run reference. Crush configuration cannot enforce
   per-step access. Note: Crush **auto-starts configured MCP servers with no
   confirmation** (Appendix A3), so operator-approved registration is the gate.
6. **Process supervision** — spawn/kill of `crush server` children, immediate
   startup adoption, quarantine logic, per-instance drain/release, and
   **client-lifecycle discipline**. Matchmaker holds one stable `client_id` per
   process, releases individual workspace holds before idle shutdown, and uses
   `DELETE /v1/clients/{client_id}` only as final whole-process cleanup. Crush
   v0.91.2 ties workspace lifetime to client claims; leaked or orphaned claims
   alter teardown and adoption behavior.
7. **Secret hygiene** — ensuring raw Crush responses, provider keys, session
   messages, tool parameters, and tool history are never logged or inserted into
   Matchmaker's SQLite store. Only required typed fields and stable references
   are retained. Explicit report export may stream session content to an
   operator-selected file outside the store (DESIGN §5.5, §9.6).
8. **Operator surfaces** — CLI argument handling, centralized terminal-safe
   rendering for CLI/TUI/errors/report previews, structured logging of bounded
   agent fields, and per-goal report generation/export.
9. **Orchestrator recovery** — adopt-vs-respawn decisions on restart; resume
   of in-flight goals by reconciling persisted session IDs against Crush's
   project-local durable session state without duplicating ambiguous dispatches.

### 4.2 Deferred to Crush

Matchmaker relies on Crush for these and does not re-mitigate them. A failure
here is a Crush vulnerability; Matchmaker's mitigations (deny-by-default
supervision, quarantine) limit blast radius only. All items verified against
the v0.91.2 source (see Appendix for endpoint-level detail).

1. **Agent sandboxing and tool gating** — the Crush permission system that
   decides what an agent may do inside its workspace (TB5). There is **no
   sandbox**: gated tools run with full user privileges; controls are the
   permission prompts, a best-effort bash blocklist, and the LSP auto-start
   denylist. Grants are in-memory only (per process, per session).
2. **LLM provider channel security** — TLS, key storage, provider selection,
   and prompt/data egress to providers (TB6). Keys are persisted by Crush at
   `~/.local/share/crush/crush.json` (mode 0600) or the workspace
   `.crush/crush.json`.
3. **Crush server API correctness** — workspace lifecycle (C1; in v0.91.2
   tied to client claims with detach grace), session semantics, event stream
   correctness, and version compatibility (C4, C5).
4. **MCP tool execution** — how Crush discovers, configures, and invokes MCP
   servers and other tools, including auto-starting configured stdio MCP
   servers as arbitrary child processes and permission-gating MCP tool calls
   (with a hardcoded Docker MCP whitelist that bypasses gating).
5. **Provider key storage on disk** — Crush's own config files.
6. **Crush's own local attack surface** — the unauthenticated shell endpoint,
   `/control`, `permissions/skip`, and config-mutation endpoints are exposed
   to any local process. Matchmaker cannot fix this; it can only avoid using
   those endpoints and keep servers on loopback (§4.3.1).
7. **Hook execution** — project-configured PreToolUse hooks are shell commands
   that can auto-approve and rewrite tool calls; they run at the operator's
   trust level and are entirely Crush's domain.
8. **Workload security isolation** — Matchmaker does not isolate agents from the
   orchestrator, other projects, or the host. Deployments that require this
   boundary must establish and operate it independently, before Matchmaker
   starts, using controls such as dedicated OS users, containers, or microVMs.
   Matchmaker does not provision, configure, validate, or depend on them.

### 4.3 Out of scope (accepted environment)

Explicitly **not defended** in this design; documented as assumptions:

1. **Co-located attacker on the host.** Any local process can connect to the
   unauthenticated loopback Crush and coordination MCP ports, impersonate the
   orchestrator or an agent, invoke unauthorized API actions that generate
   events, or grant permissions — and, worse,
   invoke Crush's **unauthenticated remote-shell endpoint** directly
   (Appendix A1), bypassing Matchmaker's supervision entirely. Mitigating
   this requires OS-level isolation (containers, per-project users, Unix
   socket + peer credentials — noting Crush's default Unix-socket mode does
   not explicitly chmod the socket) and is future work (DESIGN §8, remote
   fleets).
   *Accepted risk: Matchmaker targets a single-operator workstation or
   dedicated CI host where local processes are co-trusted.*
2. **Malicious fleet config from the operator.** The operator is trusted, but
   fleet input is still schema-validated. Paths, ports, tags, and typed
   `crush_options` are authoritative only within their documented constraints;
   free-form server flags are not supported.
   **Corollary (new from source review): fleet project paths are trusted
   code.** Creating a workspace on a path executes applicable Crush config in
   that directory tree and shell-expands values including `$(cmd)` (Appendix
   A2). Matchmaker therefore never points a server at an operator-unreviewed
   checkout. Onboarding records the operator-approved paths and content digest;
   later additions, removals, or modifications require renewed approval before
   lifecycle operations or new dispatch.
3. **Compromised Crush binary or host OS.** Supply-chain and kernel-level
   attacks are outside the model.
4. **Multi-tenancy.** The fleet is a single trust domain (DESIGN §6): any goal
   participant can note any other. There are no per-sender ACLs. The notes
   channel is cooperative and non-confidential; sender identity is unverified,
   and goal scoping is an organizational control rather than a security boundary.
5. **Network attackers** — all listeners are loopback; no network-facing
   surface exists until §8 features land, at which point this model must be
   revised (TB7).
6. **Isolation infrastructure.** Any dedicated OS users, containers, microVMs,
   network policy, or filesystem policy surrounding Matchmaker are separate
   deployment concerns with their own configuration and threat models. Their
   presence and correctness are not assumed unless an operator explicitly
   establishes them.

## 5. STRIDE analysis

Threats against in-scope components, with the mitigation Matchmaker must
implement. "Existing" means already mandated by DESIGN; "Required" means this
model adds a requirement.

### 5.1 Spoofing

| Threat | Boundary | Mitigation |
|---|---|---|
| Local process impersonates the orchestrator to a Crush server (grants permissions, submits prompts) | TB2 | *Accepted* (§4.3.1). Partial: loopback-only binding limits to local actors (existing). |
| Local process or agent impersonates another agent to the coordination MCP server | TB4 | *Accepted*: `from` and `for` are required claimed project names so addressing is implementable, but are unauthenticated. Validate participation to prevent mistakes; preserve claims for coordination without presenting them as authenticated. |
| Orchestrator adopts a foreign process listening on a fleet port as if it were a managed server | TB2/TB3 | **Required**: on adoption, verify `/v1/version` (version + `build_id`) against the approved compatibility policy and confirm the workspace path and effective typed options match the fleet entry before driving it. A mismatch blocks adoption. Note v0.91.2 deduplicates workspaces by canonicalized path and first-create-wins on `yolo`/`data_dir`/`env`; a foreign prior creator requires recreation. |
| Agent claims notes came from a trusted source | TB4 | Existing: notes are labelled "untrusted, from other agents" at injection (DESIGN §5.4). **Required**: preserve explicit claimed-sender/goal metadata in the injected section; never render notes as authenticated sender or operator instructions. |

### 5.2 Tampering

| Threat | Boundary | Mitigation |
|---|---|---|
| Agent tampers with the SQLite store directly (same OS user) | TB1 | *Accepted* (§4.3.1): the store is host-file-permission protected only; agents already have broader local access via Crush tools. |
| Malformed goal DAG corrupts dispatch state | TB1 | **Required**: validate the complete submission against one fleet snapshot before persistence: non-empty unique step IDs, existing dependencies, acyclic graph, syntactically valid and non-empty target resolution, parseable/renderable templates with a minimal function surface, recognized policies/modes, and bounded timeout/retry values. Return all errors and reject atomically without writing goal state or starting work. |
| Agent-controlled content in store (notes, run output) is read back into templates as executable input | TB1/TB4 | Existing: references-not-inlining and untrusted-labelling (DESIGN §9.5, §6). **Required**: `text/template` (not `html/template`) output is never executed by Matchmaker; note/run content is only ever data in prompts. |
| Orchestrator crashes after prompt acceptance but before recording acceptance | TB1/TB2 | **Required**: create and persist the session ID, caller-supplied `RunID`, prompt hash, and `dispatching` state before submission. Recover through the known durable Crush session. Because session history does not retain `RunID`, never automatically resubmit an ambiguous dispatch; keep it nonterminal `unknown` until reconciled or explicitly abandoned. Retry first records abandonment. |
| Matchmaker registration damages or injects executable project config | TB3 | **Required**: depend only on `crushrc`; generate one deterministic `mcp add` fragment with shell-safe literal quoting and no agent-derived values. Show it for explicit onboarding approval. Back up and exclusively lock the file, reject symlinks or concurrent changes, modify only a clearly delimited owned block, write by atomic rename, and verify Crush parsing. If safe additive modification cannot be proven, leave the file untouched and require manual installation. Commit Matchmaker's write and approved fingerprint together. |
| Crush config or startup inputs change after approval | TB3/TB5 | *Deferred to Crush* for execution semantics, but **Required** for lifecycle control: fingerprint applicable project/global shell config, legacy JSON still loaded, selected environment, binary identity, and effective data directory. Recheck before lifecycle calls; changes require renewed approval and block new dispatch. A same-user process can still race the recheck and Crush load; this TOCTOU is accepted under the no-isolation model. Approval records consent, not safety. |
| Store corruption/lock | TB1 | Existing: crash loudly, integrity-check before reconcile resumes (DESIGN §7). |
| Instance shutdown is attempted while Matchmaker's SSE claim keeps its workspace alive | TB2/TB3 | **Required**: enter `draining`, stop dispatch, await runs unless forced, close the target workspace SSE stream, release its hold with workspace-scoped DELETE, await removal, then request idle shutdown. Use process signals only after bounded teardown timeouts; never retire the process-wide client to stop one instance. |
| Orchestrator restart misses Crush's workspace detach grace | TB2/TB3 | **Required**: reconcile immediately before starting the periodic timer and attach the new process claim to matching workspaces. If expiry already occurred, recreate the workspace and reconcile runs from persisted session IDs without ambiguous resubmission. |

### 5.3 Repudiation and operational auditability

Matchmaker does not provide non-repudiation. Its SQLite store is mutable by the
invoking OS user and by any same-user process with filesystem access. Records
support debugging and reconstruction but are neither append-only nor
tamper-evident. Stronger guarantees require a separately administered append-only
or remote audit sink outside Matchmaker's trust domain.

| Threat | Boundary | Mitigation |
|---|---|---|
| Operator cannot reconstruct why a permission was granted or a run retried | TB1 | Existing: orchestration history provides operational auditability (DESIGN §5.5). **Required**: before responding, record policy, decision, request type, workspace/session/run IDs, instance generation, and Crush request ID when supplied, but not raw tool parameters. If persistence fails, deny. Do not represent the record as tamper-proof evidence. |
| Agent denies sending a note | TB4 | Existing: notes persist before delivery (DESIGN §5.2). Sender is self-asserted and retained only as best-effort coordination metadata; no non-repudiation claim is made. |
| Same-user process alters local operational history | TB1 | *Accepted*: local history is not tamper-resistant. File permissions prevent accidental cross-user access, not modification by the owner. An independently protected audit sink is outside Matchmaker's scope. |

### 5.4 Information disclosure

| Threat | Boundary | Mitigation |
|---|---|---|
| Provider keys, messages, or tool parameters leak from Crush responses into Matchmaker logs or SQLite store | TB2 | **Required**: decode only required typed fields at the client boundary and never log or insert raw responses into the store. Matchmaker stores references rather than session content. Explicit report export may stream content only to an operator-selected file outside the store. SSE permission events retain only policy, decision, request type, and correlation metadata. |
| Notes are mistaken for confidential or authenticated communication | TB4 | **Required**: document the project-wide channel as non-confidential and prohibit secrets, credentials, trusted operator instructions, and authorization decisions. Per-goal validation and active-goal checks prevent accidental misuse only; they do not authenticate callers or enforce per-step access. Projects needing no live tools opt out of registration entirely. |
| Agent retrieves an unrelated or unbounded run result | TB4 | **Required**: `result_read` accepts only a completed run belonging to the supplied goal and returns a server-bounded chunk through an opaque cursor. Goal/run checks prevent accidental misuse, not malicious access; retrieved output remains non-confidential and may contain workspace-derived secrets. |
| CLI, TUI, logs, errors, or report previews render agent content containing terminal escape sequences | TB1 | **Required**: all interactive terminal-bound agent strings use one mandatory renderer that removes C0/C1 controls except permitted layout, strips ANSI/OSC and other escape sequences, replaces invalid UTF-8, and discards agent styling. Structured logs use bounded normalized quoted fields, never interpolated message text, and never include raw payloads, prompts, note bodies, or run output. |
| Exported report preserves dangerous terminal bytes or workspace secrets | TB1 | **Required**: export only on explicit operator action to an operator-permission file; warn that original content may contain escapes and secrets; never automatically print or preview raw exports; sanitize through the same renderer whenever displayed later. |
| Store file world-readable | TB1 | **Required**: create store and config with `0600`/`0700` permissions. |

### 5.5 Denial of service

| Threat | Boundary | Mitigation |
|---|---|---|
| Agent floods coordination MCP or prompt injection exhausts store, memory, or context | TB4 | **Required**: enforce pre-allocation limits for note UTF-8 bytes, notes per goal, retained notes, injected bytes per prompt, `note_read` page notes/bytes, fixed `result_read` chunks, and requests per time window. Caller page sizes may only lower maxima. Reject atomically with stable error codes; pagination never splits a note body. |
| Crash loses or silently acknowledges notes selected for prompt injection | TB1/TB4 | **Required**: assign monotonic store-generated `note_id`s, freeze audience expansion at acceptance, and maintain a durable `(goal, instance)` high-water cursor. Advance it only in the transaction recording accepted prompt submission. At-least-once duplicate injection after a crash is acceptable; note loss is not. |
| Crash-looping instance consumes host resources | TB3 | Existing: quarantine after max restarts per window (DESIGN §5.1). |
| Timeout is marked terminal while the agent still executes, allowing overlapping work | TB2/TB5 | **Required**: transition to `cancelling`, retain serialization, stop new dispatch, and send session cancellation once. Only matching `run_complete` establishes terminal outcome; session idle is insufficient. If grace expires, restart before reconciliation; unresolved outcomes remain nonterminal `unknown` until explicit abandonment or later evidence. |
| Cancelling a Crush session discards unrelated queued work | TB2 | **Required**: Matchmaker owns its durable queue and submits only one active run per workspace; do not use Crush's queued-prompts endpoint. Retry only after the prior attempt is terminal; an `unknown` attempt requires explicit abandonment even after instance restart. |
| Goal DAG exhausts resources through size or pathological fan-out | TB1 | **Required**: configurable submission limits for total steps, dependencies per step, prompt bytes, resolved targets per step, and total expanded runs. Expand targets during validation, persist the frozen expansion and initial attempts transactionally, and reject the whole goal before dispatch if any limit is exceeded. |
| Fleet changes or retry history make fan-out aggregation nondeterministic | TB1 | **Required**: freeze target expansion at acceptance; represent each target with ordered immutable attempts; only the highest-numbered terminal attempt determines target status and downstream result reference. `unknown` is nonterminal and blocks aggregation; explicit `abandoned` is terminal and counts as failure. Step rollup waits for all targets and retry exhaustion: all success is `succeeded`, all failure is `failed`, mixed outcomes are `partial`. Dependents require `succeeded` unless `accept_partial_needs` was explicitly set. |
| Invalid or implicit state transition releases serialization or dispatches work prematurely | TB1/TB2 | **Required**: enforce persisted instance and run transition tables. `unknown`, `approval_required`, `version_mismatch`, `draining`, and `failed` are dispatch-blocking. Only explicit operator abandonment converts an unresolved run to terminal `abandoned`; transition and actor are audited. |
| Matchmaker orchestration metadata grows without bound | TB1 | **Required**: provide a prune/gc subcommand governed by operator policy. Crush separately owns retention of session messages and tool history; Matchmaker does not duplicate them. Explicitly exported reports are separate operator-owned artifacts. |

### 5.6 Elevation of privilege

| Threat | Boundary | Mitigation |
|---|---|---|
| `grant_all` automatically permits dangerous tool requests | TB2/TB5 | Existing: deny-by-default, `grant_all` is explicit per-step opt-in (DESIGN §6). **Required**: v1 gives every attempt a dedicated session because permission events lack `RunID`; correlate by workspace, dedicated session, and instance generation. Reject stale or mismatched requests, persist before responding, deduplicate request IDs, and send action `allow` only for the individual event. Never call `permissions/skip` or create a yolo workspace. |
| Delayed or replayed permission event is granted under another run's policy | TB2/TB5 | **Required**: identify each server lifetime with an instance generation and bind each SSE stream and run to it. Never infer policy from the workspace's current activity alone. Requests from an old generation or outside the exact active run fail closed to `deny`. |
| Permission grants silently reset on server restart | TB2 | **Required (new)**: Crush grants are in-memory per process (v0.91.2). After Matchmaker restarts an instance for crash recovery or approved config change, previously granted `allow_session`-style approvals are gone; supervision must re-apply the step's policy from scratch and expect renewed `permission_request` events on retry. |
| Prompt injection via notes or upstream content steers an agent into requesting dangerous permissions | TB4/TB5 | Existing: notes labelled untrusted (DESIGN §6). Deny-by-default means injected instructions cannot self-authorize. Residual risk is accepted and documented for operators choosing `grant_all`; there is no sandbox behind a grant (Appendix A1), and a project-configured PreToolUse hook can auto-approve calls without Matchmaker seeing them, so `deny` does not suppress hook pre-approvals. |
| Template injection: goal author (operator) writes a template that exfiltrates — operator is trusted, but a *future* goal-decomposition agent (DESIGN §8) would make templates agent-influenced | TB1 | **Required** (forward-looking): keep the template function surface minimal (no file/exec helpers); when §8 decomposition lands, templates become untrusted input and this control becomes mandatory. |
| Reserved `ask` policy is accepted without an operator channel | TB1/TB2 | **Required**: v1 goal validation accepts only `deny` and `grant_all` and rejects `ask` explicitly. Enable `ask` only after an authenticated, available operator channel provides strict permission-request-to-run correlation and fail-closed behavior. |
| Fleet server options override loopback binding, project path, or permission/lifecycle invariants | TB2/TB3 | **Required**: expose typed options only (`debug` and `data_dir` in v1); reject free-form flags. Matchmaker owns host, port, yolo, config, lifecycle, and profiling settings, passes project path only in workspace creation, and verifies endpoint, canonical workspace path, and effective options before adoption. |
| Crush version or `build_id` differs from the reviewed deployment pin | TB2/TB3 | **Required**: place the instance in `version_mismatch` and block adoption and dispatch. Do not restart-loop the unchanged binary. Require the operator to install/configure the approved binary or record an explicit audited compatibility override for the observed version and `build_id`; a pin change triggers Appendix review. |
| Duplicate fleet paths or data directories cause shared state and competing servers | TB3 | **Required**: canonicalize and require unique project names, loopback ports, project paths, and effective data directories before reconciliation. |
| Matchmaker spawns `crush` via PATH hijack | TB3 | **Required**: resolve the `crush` binary path at startup (absolute path or configurable), verify it is executable and check version before spawning; never re-resolve per spawn. |

## 6. Assumptions summary

1. The host is single-operator; all local processes are co-trusted (§4.3.1).
   This assumption carries more weight after the Crush source review: any
   local process has an unauthenticated remote shell on every fleet server
   (Appendix A1).
2. The operator is trusted and is the only goal submitter (until §8), and
   fleet project directories are operator-trusted code (crushrc execution,
   §4.3.2).
3. Crush's permission system works as documented; Matchmaker supervises but
   does not sandbox — and Crush itself provides no sandbox, only gating.
   Per-project servers provide fault and lifecycle isolation, not security
   isolation. Any required workload isolation is independently deployed and
   operated outside Matchmaker.
4. All listeners are loopback; there is no network-facing surface in v1.
5. Agents are prompt-injectable; the design assumes injected content will
   eventually reach an agent and mitigates via deny-by-default permissions
   and untrusted labelling rather than prevention.

## 7. Review triggers

Revise this model when any of the following land (all DESIGN §8):

- Event-driven intake (cron, webhooks) — new untrusted goal-submitters cross TB1.
- Orchestrator remote API — TB7 becomes live; authentication mandatory.
- Remote fleets — TB2 leaves loopback; §4.3.1 assumption breaks.
- Goal decomposition by an agent — templates become untrusted (§5.6).
- Multi-tenancy or per-sender note ACLs — the flat trust domain assumption (DESIGN §6) changes.
- Crush version pin bump — re-run the Appendix review against the new source.

## Appendix: Crush v0.91.2 attack-surface inventory

Source review of `github.com/charmbracelet/crush` at v0.91.2 (local clone:
`~/experiments/crush`), focused on what an orchestrator must know. Crush has
no SECURITY.md or published threat model; its server is designed under a
"same OS user = trusted" assumption. Every control (permissions, bash
blocklist, yolo) gates the *agent*, never the *API client*.

### A1. Unauthenticated API endpoints (all of `/v1`, no auth/TLS/origin checks)

| Endpoint | Capability | Matchmaker exposure |
|---|---|---|
| `POST /v1/workspaces/{id}/agent/sessions/{sid}/shell` | **Arbitrary shell command** in the workspace dir, full user env; no permission gate (`internal/backend/agent.go`) | Never called by Matchmaker; reachable by any local process (accepted, §4.3.1) |
| `POST /v1/workspaces/{id}/permissions/skip` | Enables yolo (disables all permission gating) at runtime | Never called; `grant_all` is implemented per-event instead (§5.6) |
| `POST /v1/control` (`shutdown`) | Server shutdown; in v0.91.2 both spellings are **idle-guarded** (refused while workspaces live) | After closing the target SSE stream and releasing its workspace hold, Matchmaker uses `shutdown_if_idle` per DESIGN §5.1 |
| `POST /v1/workspaces` | Caller-controlled `path` (any accessible absolute path — no jail), `yolo`, `data_dir`, `env`; first-create-wins on duplicates | Matchmaker always sets explicit values; verifies on adoption (§5.1) |
| `GET /v1/config`, `GET /v1/workspaces`, `GET .../{id}/config` | Return effective config **including resolved provider API keys**, unredacted | Scrubbed at client layer (§5.4) |
| `POST .../config/set`, `config/provider-key`, `config/model` | Mutate config; provider-key **persists keys to disk** (0600) | Matchmaker never mutates provider config |
| `GET .../sessions/{sid}/messages` | Full conversation history | Read lazily for `result_read` and reports; potentially secret-bearing; never copied wholesale into Matchmaker's store |
| `DELETE /v1/workspaces/{id}?client_id=...` | Releases one client's creation hold; live SSE streams continue holding the workspace | Used after closing the target stream during per-instance drain |
| `DELETE /v1/clients/{client_id}` (new in v0.91.x) | Retires a client claim across workspaces; workspace lifetime is tied to client claims with detach grace | Matchmaker keeps one stable `client_id` per process and retires it only as final whole-process shutdown cleanup |

### A2. Config is executable

- `crushrc` / `.crushrc` files are **shell scripts executed** by Crush's
  embedded `mvdan/sh` interpreter at config load and reload, walking from the
  workspace path up to the git root (`internal/shellconfig`). Legacy JSON
  config in the same resolution scope can define hooks, MCP processes, allowed
  tools, and other security-relevant behavior while supported, but Matchmaker
  neither writes nor depends on it. Repo-committed config applies on workspace
  creation or reload.
- Config values (`api_key`, MCP `command`/`args`/`env`/`url`/`headers`)
  undergo **shell expansion including `$(cmd)`** (`internal/config/resolve.go`).
- `.env` in the working directory is auto-loaded (godotenv import in `main.go`).
- PreToolUse **hooks** are config-defined shell commands that can auto-approve
  a tool call and rewrite its input — they bypass Matchmaker's `deny` policy.

### A3. Agents and tools

- No sandbox: gated tools (bash, write, edit, fetch, LSP, MCP) execute with
  full user privileges once granted. The bash blocklist (curl, ssh, sudo,
  package managers, …) is best-effort prompt-level hardening, not a boundary.
- Permission grants are **in-memory only** (per process; `allow_session` does
  not survive restart). `allowed_tools` in config is the only persistent grant.
- MCP servers from config are **auto-started with no confirmation**; stdio
  MCP = arbitrary child process. MCP tool calls are permission-gated except a
  hardcoded Docker MCP whitelist.
- LSP servers auto-spawn from config on file events (generic interpreters
  denylisted); also startable via the API.
- `CRUSH_PROFILE` starts a pprof server on `localhost:6060`.
- Per-workspace `.crush/` dir is created 0700 with a `*` `.gitignore`; global
  keys live in `~/.local/share/crush/crush.json` (0600).
