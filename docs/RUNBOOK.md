# replan runbook

How to install, run and troubleshoot replan, before and during re:Invent.

## 1. Install

| Need | Notes |
|---|---|
| Go 1.26+ | Only to build from source. `go build -o bin/replan ./cmd/replan` makes one binary with no other dependencies |
| A registered re:Invent pass | Its catalog and your schedule need your AWS Builder ID |
| Kiro CLI or Claude Code | Optional, for the agent |
| macOS, Linux or Windows | Sign-in opens your browser and listens on `127.0.0.1:8484–8489` |

```bash
git clone https://github.com/awsfanboy/replan.git && cd replan
go build -o bin/replan ./cmd/replan
export PATH="$PWD/bin:$PATH"
```

## 2. Sign in

```bash
replan login      # opens AWS Builder ID in your browser, PKCE, comes back to 127.0.0.1:848x
replan whoami     # who is signed in, and when the access token expires
replan logout     # revokes the refresh token and deletes it locally
```
- Access tokens last 60 minutes and refresh on their own. The refresh token lasts 30 days.
- Tokens are stored in your user config directory (macOS: `~/Library/Application Support/replan/tokens.json`), readable only by you, and sent only to `api.awsevents.com`.
- `logout` doesn't end your Builder ID browser session. To do that, visit https://profile.aws.amazon.com.

## 3. Before the event: build and book your plan

```bash
replan init --event reinvent2026     # starter reinvent.yaml
replan search                         # overview by day, venue, kind, level, topic
replan search agents --day tue --live # --live: only sessions that won't be recorded
replan ui                             # browse all sessions and build the plan in your browser
replan show SVS401                    # details, repeats, same-slot alternatives
replan plan                           # what would change, and why
replan apply                          # asks "yes", reserves in rank order, verifies
replan agenda                         # your real schedule, day by day
```

`reinvent.yaml` is the source of truth:

```yaml
event: reinvent2026
rules:
  maxPerDay: 4          # sessions per day
  arriveEarly: 10m      # reserved seats are released ~10 min before start
  travel: true          # write walking blocks into your official schedule
  prune: false          # true = cancel reservations not listed here
sessions:               # ranked: #1 matters most; each entry reserves ONE seat
  - reserve: AIM301
    fallbacks: [AIM301-R]
    why: agents in production
favorites: [SVS401]
blocks:
  - {title: Lunch, day: 2026-12-01, from: "12:00", to: "13:00", location: Wynn}
skip: []                # sessions you won't attend: their seats are given back
```

**Walk-up sessions:** breakouts and lightning talks take no reservations. Put them in `sessions:` like any
other; `plan` shows them as **walk-up: favorite**, `apply` favorites them (so they appear in the app) and adds
walking time around them. As of Oct 26, 1,382 of the 1,479 reservable sessions were full, so give every
entry a fallback, and walk-ups make good ones.

**Reserved seating:** opens on the website on Oct 6 and **through the API on Oct 8, 2026**. Before
then, `apply` reports "reservations are closed" (409) and still applies favorites and blocks. Run
it again once reservations open.

## 4. During the event: replan Live

| Situation | Command |
|---|---|
| Where next, when do I leave, will I make it? | `replan now` (add `--at "MGM Grand"` and `--late 20m` when needed) |
| Evening: anything tomorrow I can't keep? | `replan tonight` (or `--day 2026-12-03`) |
| Did anything change that affects me? | `replan changes` (downloads the catalog and records the differences) |
| I won't go to a session | `replan skip CODE && replan plan && replan apply` |
| Changed my mind | `replan unskip CODE` |
| Something moved | `replan plan`: walking blocks and fallbacks are recomputed |

`replan changes` only knows what changed between downloads replan has made. Run it (or any
command with `--refresh`) a few times a day during the event to keep the history current.

## 5. With an AI agent

```bash
replan setup kiro                    # writes ~/.kiro/agents/replan.json
kiro-cli chat --agent replan
replan setup claude                  # prints the `claude mcp add` command
```
- Auto-approved: searching, reading and editing the plan, `plan`, `whats_next`, `tonight_check`, `what_changed`.
- **Never auto-approved: `apply`.** Kiro asks you first, and replan also checks that the `plan_id` is the plan you saw.
- In clients that support MCP dialogs, replan shows its own confirmation dialog. The model can't answer it for you.
- Useful prompts: *"I'm at the Venetian and 15 minutes late, what's next?"*, *"Anything changed today?"*, *"Check tomorrow for seats I can't use."*

## 6. Simulator and demos (no real changes)

Add `--sim` to any command to use a local simulator instead of your schedule. Simulator state lives
in `.replan/` next to your plan file.

| Flag | Effect |
|---|---|
| `--sim-reset` | start from an empty schedule |
| `--sim-full COM317-R1,ARC308` | these sessions are full (the simulator is then deterministic) |
| `--sim-full-rate 0.15` | a random share of sessions are full |
| `--sim-closed` | reservations closed (409) |
| `--now "tue 12:40"` | time travel (any command, and `mcp` and `ui`) |
| `replan sim shake --sim -o shaken.json` | creates realistic event-week changes aimed at your reservations, plus `shaken-base.json` to compare against |
| `--catalog FILE --compare FILE` | use a catalog snapshot, and compare it with an older one |

`scripts/demo-reset.sh` sets up the complete recording demo in `~/replan-demo`.

## 7. Files

| What | Where (macOS) |
|---|---|
| Plan file | `./reinvent.yaml`, plus `reinvent.yaml.bak` after each edit by an agent or the UI |
| Tokens | `~/Library/Application Support/replan/tokens.json` (0600) |
| Catalog cache (1 hour) | `~/Library/Caches/replan/catalog-<event>.json` |
| Change history | `~/Library/Caches/replan/changes-<event>.jsonl` |
| Simulator state | `<plan dir>/.replan/sim-<event>.json` |
| Kiro agent | `~/.kiro/agents/replan.json` or `./.kiro/agents/replan.json` (`--workspace`) |

## 8. Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| `requires registration: run replan login` | Anonymous request to a registration-only catalog | `replan login` |
| `signed in but not registered for <event>` (403 with a body) | The Builder ID isn't registered for that event | Register on the event site. Signing in again won't help |
| `edge-refused` (403, no body) | AWS's edge is refusing your address | Slow down; reads retry on their own |
| `reservations are closed right now` (409) | Reserved seating isn't open in the API yet | Wait (re:Invent 2026: Oct 8), then `apply` again |
| `throttled` (429) | Per-minute quota: 30 reservations, 120 reads | replan waits `Retry-After` on its own |
| `outcome unknown` | A write's response was lost (500/503/timeout) | Nothing to do. replan reconciled from GetSchedule; `replan plan` shows what's left |
| `The plan changed since plan-… was shown` | The plan file, the catalog or your schedule changed after approval | Run `plan` again, review it, approve again |
| `all reserved sign-in ports 8484-8489 are busy` | Other programs are using the ports | Free one and run `replan login` again |
| A session "fell back" | The server said full or clashing | Expected: the next candidate in the entry was reserved |
| Kiro: `tool permission approval is not supported in non-interactive mode` | Kiro won't auto-approve `apply` | Run Kiro interactively and approve, or pass `--trust-tools=@replan/apply` for a scripted demo |
| Kiro (2.36+) says the replan tools "aren't available" | The folder isn't trusted yet, or the agent was written by an older replan (no `permissions` block) | Re-run `replan setup kiro`, then start Kiro **interactively** and trust the folder (or pass `--trust-workspace`). Kiro 2.36's headless `--no-interactive` mode doesn't start agent tool servers |
| `replan changes` shows nothing | No previous download to compare with yet | It fills up as replan refreshes. For demos use `sim shake` |

## 9. Fair use

replan reserves only the sessions you ranked, one request per session, when you approve it. It
never polls for seats or grabs them the moment they open. It suggests giving back seats you can't
use, and `skip` does it.
