# unring — local rollback: design and spike results

> 2026-08-06 · spike complete, implementation not started
>
> This follows [PROJECT-BRIEF.md](PROJECT-BRIEF.md) and records a shift in what the
> product leads with: from intercepting outbound side effects to making local state
> reversible. The brief's conclusions still hold; they are no longer the default path.

---

## 1. Why the shift

The first real use of v0.1.0 — wrapping an actual `claude` session — exposed a problem the
design had not anticipated. Every call to an unrecognised service asks for approval, and
only two adapters ship, so "unrecognised" means the entire world except GitHub and Slack.
One session produced nine prompts in a row.

That is not merely annoying. **A user who answers `y` nine times without being able to
evaluate any of them has been trained by us to ignore the prompt** — so when the one that
matters arrives, they will answer `y` to that too.

The judgment that follows: **an agent's value is that it runs unattended, and any mechanism
requiring per-action approval spends that value.** Snapshot-and-restore-later costs nothing
at all while the agent runs.

### What snapshots cannot do — stated first, deliberately

**A snapshot can give back what is yours. It cannot recall what has already left.**
Restoring a snapshot does not un-send delivered mail, does not make a Slack message unseen,
does not reverse a charge. "You can't unring a bell" is where this project's name comes
from, and only the staging mechanism addresses that half.

So the outbound work is not wrong — it was **sequenced** wrong. It solves the harder and
more valuable problem, but until local rollback works well, the friction it introduces
stops anyone from reaching that value.

---

## 2. Decisions

| # | Decision |
|---|---|
| 1 | Lead with **local file rollback**; outbound interception becomes optional, revisited later |
| 2 | **Our own** snapshot mechanism (APFS `clonefile`). Time Machine is an analogy for the experience, not a dependency |
| 3 | Scope: **the project tree plus a few high-risk directories** (`~/Documents`, `~/Desktop`, `~/.config`, `~/.ssh`, `~/.aws`) |
| 4 | Files require **no decision**: a session ends without asking anything, and restore happens whenever the user notices |
| 5 | Restore is **per-file** |
| 6 | A restore conflict **refuses by default**: a file changed after the session is never overwritten. Conflicts are listed and the snapshot version is written alongside; forcing requires an explicit flag |
| 7 | The database layer **stays and remains on by default**; it still decides at session end, but **asks nothing if the database was never touched** |
| 8 | Open the database transaction lazily — on the first writing statement — and warn if one stays open a long time |
| 9 | Outbound interception (HTTPS, adapters, `gh` shim) is **off by default**, enabled by a flag |
| 10 | macOS first; on Linux use reflink where the filesystem supports it, otherwise copy for real and say plainly what that costs |
| 11 | Snapshot retention is **capped by space** (~5 GB default), evicting oldest, with usage always inspectable |
| 12 | Change detection: **build the full scan first as the correctness oracle**, then add FSEvents and use the scan to find out what it missed |

### On the semantic gap between decisions 5, 6 and the database

Files can be restored individually; the database forbids partial commit. That difference is
not arbitrary: **files have no consistency requirement between them and a database does.**
Restoring three files cannot produce a state nobody can reason about, whereas committing a
database change while withholding its related external action produces exactly that —
`notified_at` set for mail that was never sent.

The cost is that a user must hold two different notions of "undo". **Making that difference
obvious is an obligation of the interface and the documentation, not an optional polish**,
and should be treated as an acceptance criterion.

---

## 3. Measurements (2026-08-06, Apple silicon, APFS, macOS 25.5)

### 3.1 Cloning

| | Result |
|---|---|
| `clonefile()` on a directory — one call, recursive | 33,478 files in **337 ms** (**10 µs/file**) |
| `cp -c -R` over the same tree, per file | 4,480 ms — **13× slower** |
| Space | five clones of a 1 GB logical tree consumed **2 MB** |
| Writes | roughly 5.66 KB per file on the per-file path |
| Copy-on-write semantics | modifying and deleting the original both leave the clone's content intact ✓ |

### 3.2 Scanning — the basis of change detection

| Files | Time |
|---|---|
| 8,990 | 64 ms |
| 33,478 | 177 ms |
| **1,372,236 (whole home)** | **34.5 s** |

### 3.3 Cost of the chosen scope

About 21,000 files: clone ~220 ms plus two scans of ~120 ms each — **under half a second**.
At twenty sessions a day that is roughly **0.15% of SSD endurance per year**, which is
negligible.

For contrast, the whole-home option that was rejected: clone ~14 s plus two scans of 34.5 s
— **about 83 seconds** — and 2.25 GB written per snapshot, around 2.7% of endurance a year.

---

## 4. What the spike overturned

### 4.1 The bottleneck is the scan, not the clone

The assumption was that cloning would dominate. It does not. This does not change the
conclusion that the scope must be narrow, but it **changes the reason** — and it points at
the upgrade path: **once FSEvents removes the need for a full scan, whole-home scope becomes
viable again.**

### 4.2 One unreadable file makes the entire tree clone fail ⚠️

`clonefile()` on a directory is all-or-nothing. A six-file tree containing a single
`mode 000` file makes the whole call return `permission denied` and produce **zero files**.

Measured: `~/Downloads` (10,957 files) and `~/Library/Caches` both fail this way, while
`~/Documents`, `~/Desktop`, `~/.ssh` and `~/.aws` succeed. Real home directories routinely
contain such files — other users' data, quarantined downloads, some application state.

**So the implementation must** try the single recursive call first, fall back to a per-file
clone when it fails, snapshot everything it can, and **state explicitly which files could not
be captured**. Never skip silently — this is §7.3's "we would rather say this part got past
us" applied to a new surface.

### 4.3 mtime alone under-reports; `ctime` must be recorded ⚠️

Any tool that restores mtime to the nanosecond — `rsync -t`, tar extraction, some build
tools — can make a content change invisible to a size-plus-mtime comparison. Measured and
confirmed: after a nanosecond-exact mtime restore, an mtime-only diff **misses the change**.

`ctime` advances on any inode change and **no userspace call can backdate it**. With `ctime`
recorded, the same evasion is caught.

The rest of the diff is verified correct: creations, deletions and **overwrites of identical
byte length** are all detected. The one false positive — a file whose mtime changed but whose
content did not — errs in the safe direction.

---

## 5. Open questions

- **FSEvents reliability.** It coalesces events and can degrade to directory granularity or
  drop them under load. This is exactly why decision 12 builds the full scan first: ship the
  fast mechanism first and you have no way to learn what it missed.
- **The real cost of the Linux fallback.** ext4 has no reflink; the time and wear of a real
  copy are unmeasured.
- **What retention actually holds.** Snapshots pin the old blocks of modified and deleted
  files. How many sessions a 5 GB cap holds under real use is unmeasured.
- **Long-lived database transactions.** Lazy `BEGIN` removes the harm for sessions that never
  write, but a session that does write still holds locks for its duration. That is a property
  of the mechanism and belongs in the documentation.

---

## 6. What happens to the existing code

None of v0.1.0's 20,000 lines is discarded:

- the Postgres shared-transaction proxy stays and remains on by default (decision 7)
- HTTPS interception, adapters, staging and the `gh` shim stay, off by default (decision 9)
- the audit log, review screen, CI and demo are reused as they are, with the file-snapshot
  layer added alongside

The tagline — `Make everything your agent does undoable.` — is unchanged, and fits better
than before.

---

## 7. A note on method

Three times during this design, measurement overturned reasoning rather than confirming it:

1. "Local rollback is already solved by git and Time Machine." The storage mechanism does
   exist, but nothing turns it into *undo that agent session*.
2. "`find -newermt` prunes by directory mtime and therefore misses in-place modification."
   The truth was that `find` on this machine is `bfs`, which rejects that time format
   outright — the 6 ms was the cost of an error, not an optimisation.
3. "A whole-home snapshot takes about 6 seconds." Extrapolated from 800 files. The per-file
   path measured 54 seconds; a single recursive `clonefile` measured 14.

**Every step of this direction has to be measured rather than reasoned about.** The V1–V6
experiments in the original brief served the same purpose.
