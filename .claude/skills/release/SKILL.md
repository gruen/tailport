---
name: release
description: Cut a tailport release — take everything on main since the last tag, pick the bump, write the notes by hand, tag, watch the build, verify against the published artifact, and close the kata release ticket.
when_to_use: >
  When the user wants to ship — "cut a release", "bump the patch version",
  "release v0.1.6", "make a new version", "publish what's on main". Also for
  finishing a release that half-landed: notes still unwritten, a build to
  re-check, a release ticket still open.
argument-hint: '<vX.Y.Z, or "patch" / "minor"> — omit to take the next patch'
allowed-tools: Read Write Bash(date +%F) Bash(git status *) Bash(git log *) Bash(git tag *) Bash(git describe *) Bash(git rev-parse *) Bash(git fetch *) Bash(git show *) Bash(git push origin v*) Bash(go build *) Bash(go vet *) Bash(go test *) Bash(gh release *) Bash(gh run *) Bash(gh workflow *) Bash(curl *) Bash(sha256sum *) Bash(shasum *) Bash(tmux *) Bash(kata quickstart) Bash(kata search *) Bash(kata list *) Bash(kata show *) Bash(kata create *) Bash(kata claim *) Bash(kata comment *) Bash(kata label *) Bash(kata edit *) Bash(kata close *)
---

# /release — main → published release

Cut a release from what's sitting on main. Gather the commits since the last
tag, turn them into notes a user can actually read, tag, and prove the thing
that published is the thing people will download.

`RELEASING.md` is the authoritative runbook for the mechanics — read it. This
skill is the process around it: what to check before, what the notes should
say, and what "verified" means here.

One thing this skill knows that `RELEASING.md` currently gets wrong: its
step 4 claims `generate_release_notes` means nothing to write by hand. That is
false in this repo and has been for every release. See "Draft the notes".

Delete/purge stay out of scope, same as `/ticket`. So does moving or deleting
a published tag: `AGENTS.md` forbids force-push without explicit authorization
for that exact action, and a tag users may already have fetched is not yours to
rewrite. `allowed-tools` pre-approves verbs, it does not remove the others —
anything not listed still hits a permission prompt, and that's the point.

<!-- Editors: the same two rules from .claude/skills/ticket/SKILL.md apply to
     the shell-injection placeholders below.

     1. Never write that syntax in prose anywhere in this file — not in a
        sentence, not inside an HTML comment like this one. Substitution runs
        over the raw file before the model sees it and does not respect
        comments, so a placeholder written as an example gets EXECUTED.
     2. Keep the real ones read-only and boring. They are preprocessing, not
        tool calls, and it is undocumented whether allowed-tools gates them at
        all. Anything with side effects belongs in Process, as a real tool
        call, where the permission matcher demonstrably applies. -->

## Today

!`date +%F`

## Shipped so far

!`git tag --sort=-v:refname`

## Unreleased on main

!`git log --oneline $(git describe --tags --abbrev=0)..HEAD`

## Blocker gate — open, prioritized, not the release ticket itself

!`kata list --max-priority 2 --status open --no-label release --agent`

## Open release tickets

!`kata list --label release --status open --agent`

## The request

$ARGUMENTS

Empty means "cut the next patch off the newest tag above."

## Process

**1. Pick the version.** Default to the next PATCH. Only depart when the user
says so or the scope forces it, and say why.

tailport is pre-1.0, and `install.sh`'s `is_breaking` gives that real teeth:

- **PATCH** (`0.1.x` → `0.1.y`) is non-breaking. Existing installs upgrade on
  their own. This is the normal path.
- **MINOR** (`0.x` → `0.y`) pre-1.0, or any **MAJOR**, is treated as
  **BREAKING**. `install.sh` *refuses* the upgrade until the user sets
  `TAILPORT_ALLOW_BREAKING=1` by hand, on every host.

So a minor bump is not a bigger patch — it's a wall the whole fleet has to
climb over manually. Never pick one to signal that a release feels
substantial. If the user asks for one, confirm they mean the upgrade refusal,
naming that cost.

**2. Check the gate.** The owner's standing rule, recorded in `j68f`: *bump
patch after all < p3 done*. The query is injected above; `count=0` means clear.

Two things that query won't tell you:

- **It only sees prioritized issues.** An issue with no priority set is
  invisible to `--max-priority`. Skim `kata list --status open --agent` by eye
  and judge.
- **Distribution work doesn't gate a release.** `j68f` made this call
  explicitly and it holds: the packaging tickets (`jtpx` AUR, `s3wn` Homebrew)
  and the `hpx2` epic are *not* blockers. Packaging follows a release; it
  can't precede it.

`--no-label release` matters: the release ticket is priority 0, so without it
the gate blocks on itself.

If a real blocker is open, stop and say which. Don't cut around it.

**3. Prereqs** (`RELEASING.md` step 1). On `main`, clean tree, synced with
origin, and green:

```sh
go build ./... && go vet ./... && go test ./...
```

Red main, dirty tree, or unpushed commits: stop. A release is a promise about
a specific commit; make sure it's the one you think.

**4. Adopt or create the release ticket.** Reuse an open one from the list
above if it names the target version — `kata claim <ref>` and cut against it.
Otherwise create one first, matching `nrdz` / `9mjw` / `yqb0` / `j68f`:

```
kata create "Release vX.Y.Z: <one-line scope>" \
  --body-file <path> \
  --priority 0 \
  --label release \
  --idempotency-key "release-vX-Y-Z-<today>" \
  --agent
```

**5. Draft the notes.** The real work, and the step most likely to be
shortchanged.

`build.yml` sets `generate_release_notes: true`, and in this repo it produces
almost nothing: commits land straight on main with no PRs to summarize, so
GitHub emits a bare "Full Changelog" link and stops. Both `yqb0` and `j68f`
recorded that lesson the hard way. **Write the notes by hand, every time.**

Source material is the commit list plus the ledger behind it. Commit subjects
carry their kata ref in parens — `fix(ui): ... (2pz4)` — so:

```
git log --oneline <last-tag>..HEAD
kata show <ref>          # for each ref the commits name
```

Read the *ticket*, not just the subject line. The subject says what changed;
the ticket says why it mattered and what the user will notice. Notes written
off subject lines alone read like a changelog and tell the reader nothing.

Sort by what a user notices, not by commit order. Plenty of what's on main
never reaches the shipped binary — `chore(aur)` packaging, `docs`,
`chore(tooling)`. Those earn one line under "Also", or nothing.

House style, matching the published v0.1.4 and v0.1.5 notes:

````markdown
<One sentence on what kind of release this is. Not a list — a characterization.>

## Heads up: <what changed>

<Only when muscle memory breaks. v0.1.5 led with `u` becoming undo, because
that's the one thing a returning user had to relearn. Say what to relearn.>

## Highlights

- **<Bold lead-in>.** <Prose. What the user sees, and why it's better.> (<ref>)

## Fixes

- <One-liners for things that just work now.>

## Also

- <Packaging/infra worth exactly one line.>

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/gruen/tailport/main/install.sh | sh
```

Existing installs upgrade automatically — this is a patch bump, so
`install.sh` treats it as non-breaking.

**Full Changelog**: https://github.com/gruen/tailport/compare/<prev>...<new>
````

Skip any section that would be filler — a one-fix patch doesn't need both
Highlights and Fixes. Refs go **bare in parens**, `(9gys)`, the way v0.1.4
does. Not `#9gys`: v0.1.5 drifted into that and GitHub renders it as a broken
issue link.

Write it to the scratchpad. It goes in the confirm, in full.

**6. Confirm, then tag.** One deliberate y/n, the way every other irreversible
thing in this project works. Show the version, the commits it covers, and the
notes in full, and be explicit that this publishes publicly. Then:

```sh
git tag -a vX.Y.Z -m "vX.Y.Z"
git push origin vX.Y.Z
```

Annotated (`-a`), always. If the tag already exists, do not re-cut: either it
published (skip to verify) or someone half-cut it — ask.

That confirm is the whole gate, so treat it as one. `allowed-tools` lists
`git push origin v*` and `git tag *` so the y/n isn't followed by a redundant
permission prompt — but those patterns match on prefix, so they'd equally
pre-approve `--force`, `-f`, or `-d`. Nothing is stopping you but this
sentence: never force-push, never move or delete a tag, never re-cut a
published one. `AGENTS.md` requires explicit authorization for that exact
action, and the confirm you just took was for a tag, not a rewrite.

**7. Watch the build.** The pushed tag triggers `.github/workflows/build.yml`:
three matrix targets (linux/amd64, linux/arm64, darwin/arm64), each with a
`.sha256`, then a release job. Version comes from ldflags — no source edit,
and the leading `v` is stripped so `--version` matches the AUR `pkgver`.

```sh
gh run list --workflow=build.yml --limit 3
gh run watch <id>
```

If it fails, stop. Report what broke and leave the ticket open.

**8. Publish the notes.**

```sh
gh release edit vX.Y.Z --notes-file <path> --draft=false --prerelease=false
```

Full release, never draft or prerelease. `install.sh`'s default path resolves
`/releases/latest/download/<asset>`, and that alias only follows the latest
*published* release — a draft is invisible to every existing install.

**9. Verify through the published artifact.** Not a local build. The point is
proving that what published is what users get.

- `gh release view vX.Y.Z` — six assets (3 binaries + 3 `.sha256`), notes
  non-empty, `draft: false`, `prerelease: false`.
- Download from the `latest` alias and check the hash against the published
  `.sha256`. Read just the hash and compare, the way `install.sh` does — it
  deliberately avoids `sha256sum -c` because the recorded filename needn't
  match the local one.
- Run `--version` **on that downloaded binary**. It prints `X.Y.Z`, no leading
  `v`.
- Then the `AGENTS.md` bar: anything user-visible gets exercised for real on
  the downloaded binary — detached `tmux`, `send-keys`, `capture-pane` — and
  you cite what you *saw*, not that it built.

Be honest about what you couldn't reach. darwin/arm64 can't be driven from a
Linux host; say that plainly rather than implying coverage you don't have.

**10. Close the ticket.**

```
kata close <ref> --done --message "<scope + what you actually observed>" --commit <sha>
```

The message carries the evidence. "It built" is not evidence.

**11. Packaging — not yours.** Both halves publish themselves, gated on
`needs: release`. The `aur` job (`18cr`) runs `scripts/aur-publish.sh`: bumps
both PKGBUILDs, recomputes all six digests, regenerates both `.SRCINFO`, pushes
both AUR repos. The `brew` job (`nqmn`) runs `scripts/brew-publish.sh`:
rewrites the formula's `url`/`sha256` and pushes it to `gruen/homebrew-tap`
with a deploy key. Both bot-commit their bump back to `main`. `brew` needs
`aur` so those two pushes can't race.

Consequences for you:

- **Watch both jobs.** They're the last in the run, after `release`.
- **`main` moves under you after a tag** — two bot commits,
  `chore(aur): bump PKGBUILDs to X.Y.Z (18cr)` and
  `chore(brew): bump formula to X.Y.Z (nqmn)`. Fetch before touching the
  branch; don't be startled by commits you didn't write.
- **A packaging failure is not a release failure.** The GitHub release has
  already published and `install.sh` users are already served. Report it
  against `18cr`/`nqmn` and fix forward. **Never re-cut the tag** for a
  packaging job — the jobs are re-runnable (`gh run rerun <id> --failed`) and
  secrets are read at run time, so a bad secret is fixable without a new tag.

Two things v0.1.6 learned the hard way, both worth knowing before you debug:

- **The AUR's web page lies for a few minutes after a push.** It served
  `0.1.5-1` while the git repo already held `0.1.6` and the job log said
  `pushed tailport 0.1.6`. The RPC `info` endpoint caches too — it returned
  `resultcount:0` for minutes after the initial import. Verify against the
  **git repo** (`git clone ssh://aur@aur.archlinux.org/<pkg>.git`), never the
  page. Do not report a publish failure off a stale page.
- **Verify the digests, not just the exit code.** `makepkg --verifysource` in
  a clone of each AUR package re-downloads every source and validates what was
  actually published; the tap's `sha256` should equal the AUR source package's
  (same tarball). This is the cheap catch for anything that publishes a digest
  users can't reproduce.

## When to stop and ask

- main is red, dirty, or ahead of origin
- a real blocker is open (see the gate's two blind spots above)
- the tag already exists
- the build fails
- the bump would be minor/major — i.e. breaking, per step 1
- there's nothing but docs/packaging/tooling since the last tag. Say the cut
  would be empty and ask whether to hold. Thin isn't automatically wrong —
  `5qzt` argues for cutting a single string fix precisely because an
  unreleased fix is invisible to `install.sh` and to the packagers — but
  which way to go is the user's call, not yours.

## Report back

The version, the URL, what's in it, and what you verified how — including what
you couldn't. One or two lines. Don't paste the notes back; they're on the
release page.
