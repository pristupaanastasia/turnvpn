---
name: lab-log
description: Record every major stage transition and every confirmed finding of the turnvpn research into LAB.md and tick PLAN.md. Use whenever a stage is reached/finished, a make-or-break question is resolved (positive OR negative), a capture/experiment yields a conclusion, or a vector is proven dead or alive — before moving on.
---

# lab-log — keep the research notebook

turnvpn is a staged research project (see `PLAN.md`). Progress and dead ends are
easy to lose between sessions. This skill makes recording them a reflex.

## When to use

Trigger this whenever any of these happen — do it **before** moving on to the
next action:

- A stage in `PLAN.md` is started or finished.
- A make-or-break / feasibility question is resolved — **record negatives too**,
  they are the most valuable (they kill a path and save future effort).
- A capture, probe, or experiment produces a conclusion.
- A vector/variant is proven dead or confirmed alive.
- A non-obvious fact about the target (VK/OK infra, signaling, SFU behaviour) is
  learned.

## What to do

1. **Append a dated entry to `LAB.md`** (repo root). Newest entries go at the
   **top** of the log section. Use this shape:

   ```markdown
   ## YYYY-MM-DD — <short title>

   **Stage:** <which PLAN.md stage> · **Verdict:** ✅ / ❌ / 🔶 partial

   - **Tried:** what was done (one or two lines).
   - **Result:** what happened, concretely (numbers, exact bytes, errors).
   - **Evidence:** how we know (tcpdump, getStats, captured frame, command).
   - **Conclusion:** what it means for the project.
   - **Next:** the single next action it unblocks or forces.
   ```

   Keep it factual. No speculation dressed as result. A negative is a full
   result — write it with the same rigor as a positive.

2. **Tick the matching checkbox in `PLAN.md`** (`- [ ]` → `- [x]`) and update the
   "СЕЙЧАС ЗДЕСЬ" marker if the current stage moved.

3. If the fact is cross-session-important (survives beyond this task), also note
   it in the `turnvpn-feasibility` memory — but `LAB.md` is the primary,
   in-repo, committed record.

## Rules

- `LAB.md` is committed; never put secrets in it (no call tokens, TURN
  credentials, session cookies, personal data). Reference a capture by what it
  showed, not by pasting raw tokens.
- One entry per distinct finding. Don't batch unrelated results into one blob.
- If a later finding overturns an earlier one, add a new entry that says so and
  links the date — do not silently edit history.
