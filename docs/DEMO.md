# Screen-recording plan: replan Live (about 2½ minutes)

**Story:** every planner gets you ready *before* re:Invent. Then Tuesday happens. replan Live is
the co-pilot for the week itself: what's next, giving back seats you can't use, and what changed,
from an AI agent, with nothing changing until you approve.

**Setting:** the real re:Invent 2026 catalog. A simulator stands in for your real schedule, a
simulated set of event-week changes is applied, and the clock is set to **Tuesday 12:40**.
Everything shown is real output.

## 1. Before you record

| ✓ | Item |
|---|---|
| ☐ | `replan login` done once (the re:Invent catalog needs your Builder ID) |
| ☐ | Kiro CLI signed in (`kiro-cli chat` works) |
| ☐ | Run `scripts/demo-reset.sh` **before every take**. It builds replan, books the demo week in the simulator, applies the changes and writes the Kiro agent to `~/replan-demo` |
| ☐ | Display at 1920×1080. Terminal font 18–20 pt, dark theme, window maximised, prompt shortened (`PS1='$ '`) |
| ☐ | Do Not Disturb on. Close Slack and mail. Hide desktop icons |
| ☐ | The repo README open in a browser tab, scrolled to the banner |
| ☐ | Recorder: macOS **⌘⇧5 → Record Entire Screen**, or OBS at 1080p/30 fps. Use a separate mic if you narrate live |
| ☐ | One dry run end to end. Kiro's wording varies a little between runs, so rehearse the prompts, not the exact replies |

## 2. Shot list

| # | Time | Screen | Do | Say |
|---|---|---|---|---|
| 1 | 0:00–0:12 | README: **banner** | Show it | "Every planner gets you ready before re:Invent. Then Tuesday happens." |
| 2 | 0:12–0:22 | README: **intro** | Scroll down | "Before re:Invent is well served. During re:Invent, nobody helps. That's replan Live." |
| 3 | 0:22–0:30 | Terminal in `~/replan-demo` | `kiro-cli chat --agent replan --trust-workspace` | "It's Tuesday, 12:40. I'm stuck at MGM." |
| 4 | 0:30–1:00 | Kiro | Type **prompt A** (below) | While Kiro calls `what_changed` and `whats_next`: "The API has no change feed, so replan records one. It knows where I am, the walks, and when reserved seats are released." |
| 5 | 1:00–1:10 | Kiro answer | Pause on it | Point at: *ARC308 moved and I missed it; I won't make COM317-R1; AMZ401-R moved to MGM, where I already am; AIM381-S at 13:30 is reachable.* |
| 6 | 1:10–1:30 | Kiro | Type **prompt B** | "Seats I can't use should go to the walk-up line. And I'll take the session I can reach." |
| 7 | 1:30–1:45 | Kiro shows the plan, then asks to approve `apply` | Approve | "Nothing touches my seats until I say yes, and only the exact plan I saw." |
| 8 | 1:45–2:00 | Kiro result | Pause on it | "Two seats given back, one reserved, verified against my live schedule." |
| 9 | 2:00–2:15 | Terminal: `replan tonight` | Run it | "In the evening, replan tonight finds tomorrow's seats I can't keep, before the line forms." |
| 10 | 2:15–2:30 | README: **architecture diagram** | Scroll to Design | "Runs on your laptop. Builder ID. REST underneath, MCP on top. Every planner gets you to re:Invent. replan gets you through it." |

**Prompt A**
> I'm at MGM Grand and stuck here for 20 minutes. Did anything change that affects me, and what can I still make today?

**Prompt B**
> Give back ARC308 and COM317-R1 so someone in the line gets them, and reserve AIM381-S instead. Show me the plan first.

**What you should see** (from a dry run of the same state):
```
ARC308     12:30 Encore      missed: under way elsewhere   (it moved from 11:30)
COM317-R1  13:00 Encore      ✗ won't make it: you'd arrive 13:35
           ↳ instead: AIM381-S 13:30 MGM Grand · AIM385-S 14:30 MGM Grand · ARC202-R2 14:30 Caesars Palace
AMZ401-R   15:00 MGM Grand   ✓ leave by 14:40              (it moved from Caesars Forum)
New launch: KEY301-NEW Tue 16:30 Venetian fits your day

plan:  - ARC308, - COM317-R1 (seat goes back to the walk-up line), + AIM381-S
apply: ✓ cancel ARC308 · ✓ cancel COM317-R1 · ✓ reserve AIM381-S · walking block removed
```

## 3. If something goes wrong on camera

| Problem | Do this |
|---|---|
| Kiro answers differently, or calls tools in another order | Fine, keep going. If it skips a tool, ask: "Use what_changed." |
| Kiro tries to apply without showing the plan | It can't: apply isn't auto-approved. Decline, then say "Show me the plan first." |
| "The plan changed since … was shown" | Correct behaviour. Ask Kiro to run plan again and approve the new one |
| Anything else | Stop, run `scripts/demo-reset.sh`, retake from shot 3 |
| Kiro unavailable | Record the terminal version instead: `replan changes`, `replan now`, `replan skip …`, `replan plan`, `replan apply` with the flags `scripts/demo-reset.sh` prints |

## 4. After October 8 (the strongest shot)

Reservations open through the API on Oct 8. Add a 15-second shot on your **real** schedule:

1. In a real plan file, rank two sessions at different venues on the same day. Run `replan plan`, then `replan apply` (no `--sim`).
2. On your phone, open the official re:Invent app → My schedule. **Film the "Walk: Wynn → Caesars Forum" block** that replan added.
3. `replan skip CODE && replan apply`, then show the session disappear from the app. That's a seat given back.

Mirror the phone with QuickTime (File → New Movie Recording → choose the iPhone) to capture it at full quality.

## 5. Editing

- Cut waiting time while tools run, or speed it up 2×. Keep the answer frames on screen for 3–4 seconds.
- Add captions for shots 5 and 8. Zoom in on: **missed**, **✗ won't make it**, **seat goes back to the walk-up line**, **verified**.
- End card: the repo URL and "replan Live: every planner gets you to re:Invent; replan gets you through it."
- Export at 1080p H.264, under 3 minutes.
