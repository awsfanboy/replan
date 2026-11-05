![replan Live: a re:Invent co-pilot for the week itself](docs/images/banner.jpg)

# replan Live: your re:Invent co-pilot for the week itself

**▶ [Watch the 2-minute demo](https://www.youtube.com/watch?v=3d_HtFZkRKI)**

Every planner helps you **before** re:Invent: find sessions, build a list, book seats. **replan
helps during the event**, when real life happens:

- **"I'm stuck at MGM and 20 minutes late. What can I still make?"** replan knows where your
  sessions are and how long the walks take. It tells you what you'll still make, when to leave,
  and which sessions you can still reach instead.
- **Give seats back.** Many reserved seats go unused while people wait in the walk-up line.
  When you can't make a session, replan gives the seat back: right away, or the evening before
  with `replan tonight`, which finds tomorrow's reservations you can't realistically keep.
- **"What changed for me?"** The Events API has no change feed, so replan records one. You see
  sessions that moved, relocated or were cancelled, and **new-launch sessions added after the
  keynotes**, with what each means for *your* day.
- **Agent-first, with safe writes.** Ask Kiro or Claude in plain language. The agent can read and
  suggest. Nothing touches your seats until you approve the exact plan.

replan works alongside the tools you already use to choose sessions, including the official
catalog and community planners. Pick
anywhere. replan keeps the week working.

```
you   › I'm stuck at MGM Grand and can't leave for 20 minutes. Did anything change, and what can I still make?
kiro  › what_changed · whats_next
        AMZ401-R moved to MGM Grand and ARC308 moved to 14:30, so they now overlap.
        New launch: KEY301-NEW (Tue 16:30, Venetian) fits your day.
        14:30 ARC308   Encore     ✓ leave by 13:45
        15:00 AMZ401-R MGM Grand  ✗ can't make it: give the seat back?
        Instead at 16:30: COM345 (Encore), KEY301-NEW (Venetian), SVS333-R1 (Encore)
```
*(a real Kiro CLI 2.24 session against the real re:Invent 2026 catalog, using the simulator's
time travel and a simulated change set: `replan sim shake`)*

## During the event

```bash
replan now --at "MGM Grand" --late 20m     # what's next, leave-by times, will you make it, alternatives
replan tonight                             # tomorrow's seats you can't realistically keep
replan changes                             # what changed in the catalog that matters to you
replan skip AMZ401-R && replan plan        # give a seat back (apply cancels it; it is never re-booked)
```

Every command takes `--now "tue 12:40"` (time travel) for demos and what-ifs. To try the whole
week in the simulator, see "Try it" below.

| MCP tool | What it answers |
|---|---|
| `whats_next` | Remaining sessions today, leave-by times, on-track / tight / seat-at-risk / won't make it, reachable alternatives |
| `tonight_check` | Reservations for tomorrow that clash, can't be walked to in time, are repeats held twice, or collide with your meetings |
| `what_changed` | Moves, relocations, cancellations and new launches that touch your reservations, plan, favorites or interests |
| `edit_plan_file` `skip` | Mark sessions you won't attend; the next approved apply gives the seats back |

Plus an `im_running_late` prompt.

## Why this exists

- **Seats go in minutes.** You need a ranked list with backups, applied in order, falling
  back automatically when a session is full.
- **The campus is about an hour end to end.** A plan that ignores the walk from Wynn to Caesars
  Forum is not a plan. `replan` checks every transition. It also writes a walking block into
  your official schedule for each venue change, so the re:Invent app tells you when to leave.
- **Agents shouldn't hold raw write access to scarce seats.** The desired state lives in a file you
  can read. Nothing touches the real schedule until you approve, and every result is verified.
- **Schedules drift.** Sessions move, seats open up, you book something on your phone. Run
  `plan` again and it tells you.

## Use it with an AI agent (MCP)

### Kiro CLI

```bash
go build -o bin/replan ./cmd/replan
bin/replan login                  # AWS Builder ID, on your machine only
bin/replan setup kiro             # writes ~/.kiro/agents/replan.json
kiro-cli chat --agent replan
```

Tested with Kiro CLI 2.24 and 2.36. On 2.36 (the new agent engine), start Kiro interactively and trust the
folder when asked, or pass `--trust-workspace`.

`replan setup kiro` writes a Kiro custom agent that carries its own replan MCP server and sees only
replan's tools. It **auto-approves the read-only tools and plan edits, and never `apply`**, so Kiro
asks you before anything touches your seats. On top of that, replan still requires the approved
`plan_id`. Add `--sim` for a demo agent that changes nothing real, and
`--workspace` to write `.kiro/agents/` in the current directory. An example agent file is in
[`examples/kiro/replan.json`](examples/kiro/replan.json).

Tested with Kiro CLI 2.24 and 2.36. On the re:Invent 2026 catalog with the simulator, the agent
found what changed, worked out what was still reachable from MGM Grand, proposed giving a seat
back, and `apply` waited for approval. When a session was full, replan took its fallback,
verified the result, and honestly reported anything it could not book.

### Claude Code, Claude Desktop, Kiro IDE, Cursor

```bash
bin/replan setup claude           # prints: claude mcp add replan -- /abs/bin/replan mcp -f /abs/reinvent.yaml
```

JSON configs for Claude Desktop and the Kiro IDE are in [`examples/mcp/`](examples/mcp).

| Tool | What it does | Changes anything? |
|---|---|---|
| `list_events` | AWS events and whether their catalog needs registration | no |
| `search_sessions` | Free text plus filters (day, kind, venue, level, `live_only` = not recorded) | no |
| `get_session` | Abstract, speakers, seats, repeats, and same-slot alternatives (the fallbacks) | no |
| `get_plan_file` | The ranked plan in `reinvent.yaml` | no |
| `edit_plan_file` | Add or re-rank sessions, fallbacks, favorites, blocks, rules; validates every code | the file only |
| `plan` | Diff between the plan and your real schedule, with reasons, plus a `plan_id` | no |
| `apply` | Executes an **approved** `plan_id`: reserve, fall back, walking blocks, verify | **yes** |
| `get_agenda` | Your real schedule, day by day | no |

Plus a `plan_my_week` prompt that runs the whole interview → plan → approve → apply loop.

**How approval works.** `apply` takes the `plan_id` the user saw. If the plan file, the catalog or
the schedule changed since then, it refuses. It works like a saved Terraform plan.
- If the client supports MCP elicitation, the **human** confirms in a dialog. The model cannot
  answer it, and a model-supplied `user_confirmed` is ignored.
- On clients without dialogs, the agent must pass `user_confirmed: true` after an explicit yes
  in the conversation.

## Browse the catalog in your browser

```bash
bin/replan ui                     # opens http://127.0.0.1:<port>/ (add --sim to change nothing real)
```

All 2,171 re:Invent sessions on one page, filtered instantly:
- Search as you type, plus filter chips for day, venue, kind, level and topic.
- Toggles for "only not recorded", "hide unscheduled" and "hide sessions that clash with my plan".
- Results load in batches of 50; scroll for more.
- Every session has **+ Plan**, **+ Fallback** and ★.
- Session details show the repeats and similar sessions in the same time slot, ready to use as fallbacks.
- The ranked plan sits next to the results. You can reorder it, add blocks and set rules.
- **Preview plan** shows the reservations, the walking blocks and your week. **Apply** runs only after a
  confirmation step.

The page uses the same MCP tools as the agent, so the same safety rules apply. It listens on 127.0.0.1
only, checks the Host header, and needs the per-run token in the URL replan prints.

## Use it from the terminal

```yaml
# reinvent.yaml
event: reinvent2026
rules:
  maxPerDay: 4
  travel: true        # write walking time into your official schedule
  arriveEarly: 10m    # reserved seats are released ~10 min before start
sessions:             # ranked: #1 matters most
  - reserve: AIM301
    fallbacks: [AIM301-R]
  - reserve: DEV402
favorites: [SVS401]
blocks:
  - {title: Lunch, day: 2026-12-01, from: "12:00", to: "13:00", location: Wynn}
```

```bash
replan login
replan search                                  # overview: sessions per day, venue, kind, level, topic
replan search serverless --day tue --live      # --live: skip sessions that will be recorded
replan search --topic bedrock --kind workshop,builders-session --venue "caesars forum"
replan search agents --page 2                  # 25 per page; -n 0 for all; --json for scripts
replan show SVS401
replan plan
replan apply
replan agenda
```

## Try it

`--sim` swaps your real schedule for a local simulator that follows the API's documented rules:
full sessions, clashes, closed reservations and lost responses. Nothing real is ever changed.

### On the re:Invent 2026 catalog

The re:Invent catalog needs a Builder ID that is registered for re:Invent. Most real seats are
already taken, so `--sim-open-seats` lets the simulator treat them as free.

```bash
go build -o bin/replan ./cmd/replan
./bin/replan login
F=examples/reinvent-2026.yaml
./bin/replan plan  -f $F --sim --sim-open-seats --sim-reset
./bin/replan apply -f $F --sim --sim-open-seats --sim-full COM317-R1   # COM317-R1 is full: watch it take the Thursday repeat
./bin/replan plan  -f $F --sim --sim-open-seats                        # No changes.

# replan Live: simulate event-week changes, then jump to Tuesday 12:40, stuck at MGM Grand
./bin/replan sim shake -f $F --sim --sim-open-seats -o /tmp/shaken.json
./bin/replan changes -f $F --sim --sim-open-seats --catalog /tmp/shaken.json --compare /tmp/shaken-base.json
./bin/replan now -f $F --sim --sim-open-seats --catalog /tmp/shaken.json --now "tue 12:40" --at "MGM Grand" --late 20m
```

To set up the exact scene from the demo video, with a ready-made Kiro agent, run
[`scripts/demo-reset.sh`](scripts/demo-reset.sh), then:

```bash
cd ~/replan-demo && kiro-cli chat --agent replan --trust-workspace
```

## Built for the API's sharp edges

| API behaviour | What replan does |
|---|---|
| Bulk writes return `200` with per-session failures | Reserves one session per request in rank order and reads every failure code. On `sessionFull`, `scheduleConflict` and similar, it moves on to the next fallback. |
| No idempotency key; a retried reservation reports `alreadyScheduled` as a failure | Never re-sends a write whose outcome is unknown. It reconciles from `GetSchedule` instead. |
| `CreatePersonalTime` returns 204 with no ID | Tags every entry it manages (`[replan:key]` in the description). Later runs update or remove only their own blocks and clean up duplicates. Your own entries are never touched. |
| Personal time is UTC, whole minutes, 5-minute lengths; session times are local | Converts and rounds everything. Timezones come from each session rather than the event, because some event-level zones are wrong. |
| `409` while an operation is closed (reservations open via the API on Oct 8) | Stops reserving, still applies favorites and personal time, and tells you to re-run. |
| `429` with `Retry-After`; quotas count sessions, not requests | Waits exactly `Retry-After`. A throttled write did nothing, so retrying it is safe. |
| `403` means "not registered" with a JSON body, or an edge refusal with no body | Tells the two apart and never loops on either. |
| `500`/`503` | Retries reads with backoff. Treats writes as "outcome unknown" and reconciles. |
| Pages can come back short | Only an absent `nextToken` ends pagination. |
| No search or filter endpoint | Local catalog cache, search and filters. |
| Breakouts and lightning talks take no reservations (`isReservable: false`, 794 sessions at re:Invent 2026); before Oct 8 the flag was missing entirely | Plans them as **walk-ups**: favorited so they show in the app, counted for walking time, never reserved. A missing flag means "let the server decide". |
| Messy catalog data: `"Breakout"`, `"Breakout Session"` and `"Sesión grupal"`; venue only inside the room text; speakers as one `"Name, Title, Company"` string | Normalizes session types, recovers venues from rooms, splits speaker strings, and links repeats (`-R`, `-R1`). |

## Design

![How replan works: you, your laptop and AWS](docs/images/architecture.png)

Inside the binary, by package:

```
            AI assistant (Claude / Kiro / Cursor)        you, in a terminal
                         │ MCP (stdio)                           │
                  internal/mcpserver                        cmd/replan
                         └──────────────┬───────────────────────┘
      reinvent.yaml ── internal/spec    │
      catalog cache ── internal/catalog ┼─ internal/plan  ── rank · fallback · walking time · diff
      travel matrix ── internal/venues  │
                                        └─ internal/apply ── reserve → reconcile → verify
                                                  │
              internal/api (REST, typed errors) ──┴── internal/sim (local simulator)
              internal/auth (Builder ID PKCE on localhost:8484-8489)
```

Everything runs on your machine. The API requires this: sign-in works only on local ports, so
there is no server and no database, and nobody else ever holds your token. Tokens are stored in a
file only you can read and are sent only to `api.awsevents.com`.

Allocation is deliberately explainable. Entries are placed in rank order. Each takes the first
candidate that is reservable, not full, not started, under the day cap, and leaves enough time to
walk to and from its neighbours. Every skipped candidate is reported with its reason.

## Tests

```bash
go test ./...                               # unit and end-to-end, including an in-memory MCP client
REPLAN_SMOKE=1 go test ./internal/smoke -v  # the real binary over stdio against the live re:Invent catalog (after replan login)
```

The tests cover: throttling, 200s with partial failures, lost responses, duplicate-free personal
time, closed reservations, stale plans being refused, and dialogs that the model cannot bypass.

## Fair use

`replan` makes one request per session, in the order you ranked them, when **you** approve it.
It does not poll for seats or snipe reservations the moment they open. Reserved seating is a
shared resource. With `prune: true`, `replan` also gives seats back when you drop a session.

## License

MIT. See [LICENSE](LICENSE).
