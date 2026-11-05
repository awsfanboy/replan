# replan technical architecture

replan is a single Go binary that runs on the attendee's machine. It keeps the attendee's
re:Invent schedule in step with a plan file, safely, through the AWS Events API. It serves three
front ends: a CLI, a local browser UI, and an MCP server for AI agents.

![How replan works: you, your laptop and AWS](images/architecture.png)

## 1. Constraints that shaped it

| AWS Events API fact | Consequence |
|---|---|
| Sign-in is OAuth with PKCE, redirect only to `127.0.0.1/localhost:8484–8489`, no hosted option | Everything runs locally: no backend, no database, no shared tokens |
| Bulk writes return `200` with per-session failures; no idempotency key | Reserve one session per request, read every failure, reconcile unknown outcomes from `GetSchedule`, verify at the end |
| `CreatePersonalTime` returns 204 with no ID | Tag managed blocks (`[replan:key]`) so later runs can find, update and dedupe them |
| No search or filter; no change feed | Local catalog cache, search and facets; a locally recorded change history |
| Personal time is UTC with 5-minute lengths; sessions use local time; some event time zones are wrong | Normalize all times; trust each session's own time zone |
| 409 while an operation is closed; 429 with `Retry-After`; per-session quotas | Typed errors; honour `Retry-After`; keep going with favorites and blocks when reservations are closed |

## 2. Components

```mermaid
flowchart LR
  subgraph Clients
    T[Terminal]:::c
    B[Browser]:::c
    A[AI agent<br/>Kiro · Claude]:::c
  end
  subgraph replan[replan binary · your laptop]
    CLI[cmd/replan<br/>CLI]
    WEB[internal/web<br/>local UI 127.0.0.1]
    MCP[internal/mcpserver<br/>11 tools · 2 prompts]
    SPEC[(reinvent.yaml<br/>internal/spec)]
    CAT[internal/catalog<br/>cache · normalize · search · changes]
    PLAN[internal/plan<br/>rank · fallback · walk · diff]
    LIVE[internal/live<br/>now · tonight · impact]
    APPLY[internal/apply<br/>reserve → reconcile → verify]
    VEN[internal/venues<br/>walking matrix]
    API[internal/api<br/>REST client]
    AUTH[internal/auth<br/>Builder ID PKCE]
    SIM[internal/sim<br/>simulator · shake]
  end
  subgraph AWS
    OA[oauth.awsevents.com]
    EV[api.awsevents.com]
    APP[Official re:Invent app]
  end
  T --> CLI
  B --> WEB --> MCP
  A -- stdio --> MCP
  CLI --> PLAN & LIVE & APPLY
  MCP --> SPEC & PLAN & LIVE & APPLY
  PLAN --> SPEC & CAT & VEN
  LIVE --> CAT & VEN & PLAN
  APPLY --> API
  APPLY -. demo/tests .-> SIM
  CAT --> API
  API --> EV
  AUTH --> OA
  EV --> APP
```

The browser UI is an MCP client of the same server (in memory), so the UI, the CLI and agents all
share one set of tools and one set of safety rules.

## 3. Plan → approve → apply

```mermaid
sequenceDiagram
  actor U as You
  participant AG as Agent / UI / CLI
  participant R as replan
  participant E as Events API
  AG->>R: edit_plan_file (rank, fallbacks, skip)
  AG->>R: plan
  R->>E: GetSchedule
  R-->>AG: changes + reasons + plan_id
  AG-->>U: shows the plan
  U->>AG: approve (dialog / Kiro prompt / "yes")
  AG->>R: apply(plan_id)
  R->>R: rebuild plan, plan_id must match
  R->>E: CancelReservation (skips: seats given back)
  loop each ranked entry, in order
    R->>E: ReserveSessions([one id])
    E-->>R: 200 {successful, failed[code]}
    alt full / clash / not reservable
      R->>E: next fallback
    else outcome unknown
      R->>E: GetSchedule (reconcile, never re-send)
    end
  end
  R->>E: favorites, then personal time from seats actually held
  R->>E: GetSchedule (verify)
  R-->>AG: verified result
```

## 4. replan Live data flow

```mermaid
flowchart LR
  F[Catalog download] --> D{Diff with<br/>previous copy}
  D --> H[(changes-event.jsonl)]
  H --> I[Impacts:<br/>your reservations · plan · favorites<br/>new launches · interests]
  S[GetSchedule] --> N[Next:<br/>where you are · late · walks<br/>10-min seat release]
  S --> TN[Tonight:<br/>overlaps · impossible walks<br/>repeats held twice · meetings]
  N --> ALT[Reachable alternatives]
  N & TN & I --> SK[skip in reinvent.yaml] --> P[plan → approve → apply<br/>seat goes back to the walk-up line]
```

## 5. Safety layers

1. **The agent edits a plan, never seats.** MCP has no raw reserve tool.
2. **Client approval.** `replan setup kiro` never auto-approves `apply`. MCP dialogs, where supported, can't be answered by the model.
3. **Approved-plan fingerprint.** `apply` recomputes the plan and refuses if its `plan_id` differs.
4. **One session per request, every failure code read.** A full session moves to the next fallback.
5. **No blind retries.** Lost responses are reconciled from `GetSchedule`; creates are never re-sent.
6. **Managed markers.** replan only touches personal time it created.
7. **Verification.** A final `GetSchedule` confirms everything that is reported.
8. **Local only.** Tokens are 0600 and sent only to `api.awsevents.com`. The UI listens on 127.0.0.1 with a Host check, a per-run token and a CSP.
9. **Fair use.** No polling or sniping; seats you can't use are suggested for give-back.

## 6. Packages

| Package | Responsibility | Tests |
|---|---|---|
| `internal/api` | REST client, typed errors, throttling, unknown-outcome writes, pagination | 7 |
| `internal/auth` | Builder ID PKCE sign-in, refresh, revoke, token store | – |
| `internal/catalog` | Cache, normalization (kinds, venues, speakers, time zones), search, facets, change diff and history | 3 |
| `internal/venues` | Venue detection from room names, walking matrix | – |
| `internal/spec` | `reinvent.yaml` schema, validation, save with backup | – |
| `internal/plan` | Greedy rank-order allocation, walking blocks, personal-time diff, `plan_id` | 7 |
| `internal/apply` | Execute, fall back, reconcile, verify; blocks from seats actually held | 5 |
| `internal/live` | Next, Tonight, Impacts | 5 |
| `internal/mcpserver` | 11 MCP tools, 2 prompts, approval flow | 8 |
| `internal/web` | Local browser UI (embedded, no build step) over in-memory MCP | 2 |
| `internal/sim` | Simulator following the documented API behaviour; `Shake` for demos | via others |
| `internal/setup` | Kiro agent and Claude Code configuration | 3 |
| `cmd/replan` | CLI | smoke test |

About 8,200 lines of Go and 41 tests, including an in-memory MCP client, the binary over stdio
(`REPLAN_SMOKE=1`), and real Kiro CLI sessions recorded during development.

## 7. Key decisions

| Decision | Why |
|---|---|
| Desired state in a file, Terraform style | It can be reviewed, diffed and edited by humans and agents, and re-runs only fix what's off |
| Greedy allocation in rank order, not an optimizer | Explainable: every skipped candidate has a one-line reason |
| Walking blocks as personal time | It's the only way to put guidance into the official app people already carry |
| `skip` instead of a raw cancel tool | Giving seats back goes through the same plan → approve → apply loop |
| A simulator that follows the docs | Anyone, and the tests, can exercise full sessions, clashes, 409s and lost responses without touching a real schedule |
| Time travel (`--now`) and `sim shake` | The event happens after the deadline, so the week has to be demonstrable beforehand |
| The UI as an MCP client | One code path for every front end, so the safety rules can't drift between them |
