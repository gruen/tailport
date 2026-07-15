---
name: ticket
description: Turn a feature request, bug report, or idea into a well-formed kata issue in the tailport ledger — search first, fold into an existing issue when one fits, and push back when the request is unclear or collides with the project's design constraints.
when_to_use: >
  When the user describes a feature, enhancement, or bug they want tracked in
  kata — whether they invoke /ticket explicitly or just say something like "we
  should add X", "file that", "X is broken", or "put that in kata". Also when
  triaging a batch of requests into the ledger at once.
argument-hint: <the request, in your own words>
allowed-tools: Read Bash(date +%F) Bash(kata quickstart) Bash(kata search *) Bash(kata list *) Bash(kata show *) Bash(kata ready *) Bash(kata create *) Bash(kata comment *) Bash(kata label *) Bash(kata edit *)
---

# /ticket — request → kata issue

Intake for the tailport kata ledger. Take a request in whatever form it
arrives and land it as a well-formed issue — or fold it into one that already
exists, or push back if it isn't ready to be a ticket yet.

Close, delete, and purge are deliberately out of scope. This skill files work;
it does not finish it. Closing has a verification bar (`AGENTS.md`) that intake
has no business clearing, and `AGENTS.md` forbids delete/purge without explicit
authorization for that exact action and ref.

That scope lives here as an instruction, not a sandbox: `allowed-tools` only
pre-approves the verbs it lists, it does not remove the others. Deliberately
leaving `close`, `delete`, and `purge` off the list means they still hit a
permission prompt. Don't reach for them from here.

<!-- Editors, two rules about the shell-injection placeholders below (the
     bang-backtick form used under "Today" and "Current ledger"):

     1. Never write that syntax in prose anywhere in this file — not in a
        sentence, not even inside an HTML comment like this one. Substitution
        runs over the raw file before the model sees it and does not respect
        comments, so a placeholder written as an example gets EXECUTED. An
        earlier revision of this very comment broke the skill that way.
     2. Keep the real ones read-only and boring. They are preprocessing, not
        tool calls, and it is undocumented whether allowed-tools gates them at
        all. Anything with side effects belongs in the Process section as a
        real tool call, where the permission matcher demonstrably applies. -->

## Today

!`date +%F`

## Current ledger

!`kata list --agent`

## The request

$ARGUMENTS

If that is empty, the request is whatever the user just described in
conversation. Use their words, not a paraphrase you invented.

## Process

**1. Search before creating.** Always, even when you are confident it's new.

```
kata search "<keywords>" --agent
```

Try more than one phrasing — the user's words and the codebase's words often
differ (they say "copy button", the ledger says "OSC 52 clipboard"). Check the
ledger above for near-neighbours too. If kata soft-blocks a look-alike on
create, that is the feature working: stop and reconsider rather than reaching
for `--force-new`.

**2. Check it against the design constraints.** Read the "Design constraints"
section of `AGENTS.md` before filing anything that touches behavior. Those
constraints say *do not relax without asking* — so a request that collides with
one is a conversation, not a ticket. Live ones to watch: tailnet-first (funnel
is opt-in only, behind a confirm, never implicit); serve is plain HTTP, no TLS;
serve mapping is strictly 1:1; zero non-Go runtime dependencies in the shipped
binary.

**3. Decide: fold, cut, or push back.**

*Fold* when an existing issue already owns the scope. Add to it instead of
creating a sibling — `kata comment <ref>`, `kata label add`, or `kata edit`.
Say which issue you folded into and why.

*Push back* — discuss before filing — when any of these hold:

- It collides with a design constraint. Name the constraint and ask whether
  they mean to relax it.
- It's a bundle, not a ticket — several independent changes that would each
  want their own verification. Propose the split.
- You cannot write a "Done when" line. If done is unclear, the ticket is
  unclear.
- A near-duplicate exists and folding is arguable. Propose, don't assume.
- It reads as musing rather than a request ("wouldn't it be cool if…").

Push back in prose, or with `AskUserQuestion` when there are real alternatives
worth comparing. Do not file a vague ticket to avoid the conversation.

*Cut* otherwise. A clear, scoped, constraint-compatible request does not need a
confirmation round-trip — file it and report what you filed.

**4. Cut the ticket.**

```
kata create "<title>" \
  --body-file <path> \
  --priority 2 \
  --idempotency-key "<slug>-<today>" \
  --agent
```

Conventions:

- `--priority 2` is the intake default. The scale is `0..4`, **0 = highest**.
  `0` is release-critical, `1` is urgent/next-up, `2` is ordinary feature work,
  `3` is someday, `4` is an epic or bucket. Only depart from `2` when the
  request itself justifies it, and say why when you do.
- `--idempotency-key` is `<short-slug>-<YYYY-MM-DD>` using the date injected
  above, e.g. `dark-mode-toggle-2026-07-15`. It makes a retry safe.
- `--agent` on every read and mutation. `--json` only when piping to a script.
- Wire relationships when they are real, not decorative: `--parent` (sub-task
  of a larger issue), `--blocks` / `--blocked-by` (ordering), `--related`
  (context, no ordering). An epic like `hpx2` wants `--parent`; a keybind
  change that must land before a release cut wants `--blocks`.
- Reuse labels already in the ledger. Don't invent a taxonomy without asking.
- Don't set `--owner`. Ownership means "actively being worked" here, and intake
  isn't work — the person who starts it claims it.

## Ticket body — house style

Match the existing agent-written issues (`s3wn`, `hpx2`). Write prose that a
teammate could act on cold, not a restatement of the title. Concrete beats
abstract: name the files, flags, and refs.

Open with one or two sentences of what and why, with related refs inline. Then
whichever of these sections carry weight — skip the ones that would be filler:

```markdown
<One-sentence what + why. Companion to <ref> if it has a sibling.>

## Recommended plan
1. <Concrete approach. Name files, flags, functions.>
2. <ALTERNATIVE: … when there's a real fork worth recording.>

## Deliverables
- <Artifacts: files added/changed, README updates, CI changes.>

## Verification
<How this gets proven, per the AGENTS.md bar: go build/vet/test at minimum,
plus a real exercise for anything user-visible — tmux send-keys against the
compiled binary, or a go run harness against live tailscaled. Note honestly
if something can't be verified in-env.>

## Decisions to confirm
- <Open questions for the user. Omit if there are none.>
```

Cross-reference other issues by bare short_id in prose (`jtpx`, `s3wn`), the
way the existing tickets do. Use absolute dates, never "yesterday" or "last
week".

## Report back

Say what you did in one or two lines: the ref and title you created, or the
issue you folded into and why. If you pushed back, say what you need before it
can be filed. Don't paste the whole ticket body back at the user — they can
`kata show <ref>`.
