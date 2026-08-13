# unring — local rollback: design and spike results

> 2026-08-06 · first round, implemented on `local-file-rollback`
> 2026-08-09 · second round (§8), design agreed, implementation not started
>
> This follows [PROJECT-BRIEF.md](PROJECT-BRIEF.md) and records a shift in what the
> product leads with: from intercepting outbound side effects to making local state
> reversible. The brief's conclusions still hold; they are no longer the default path.
>
> §8 adds a whole-volume snapshot as a backstop and revises decisions 2 and 3. Read
> §§1–7 first: they remain accurate, and §8 says exactly which parts of them it changes.

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

---

## 8. Second design round — the volume-snapshot backstop (2026-08-09)

> Design agreed, implementation not started. Sections 1–7 above stand as written; this
> section revises decisions 2 and 3 and adds decisions 13–18.

### 8.1 What prompted it

The chosen scope is narrow by design, and the question that exposed its limit was concrete:
what happens when the agent decides disk space is low and deletes a directory of photos?
`~/Pictures` is not in scope, so nothing is captured and nothing is even reported.

The proposal was to intercept: pause the agent at the moment it deletes or modifies a file,
copy the file aside, then let it proceed. That is the right instinct and it is not
achievable on macOS. What replaces it is cheaper and covers more.

### 8.2 Interception was measured, not assumed

| Mechanism | Result |
|---|---|
| `DYLD_INSERT_LIBRARIES` interposing `unlink` | **Bypassed on `/bin/rm`** — SIP strips the variable for protected binaries. Worked only on an unprotected copy of the same binary |
| EndpointSecurity `AUTH_UNLINK` | Correct semantics, but requires an Apple-granted entitlement, root, and notarisation |
| FUSE | Requires a kext and reduced-security boot on Apple silicon |

`rm` is the most likely way an agent deletes a file, and library interposition cannot see
it. Under §7.3 of the brief — never silently fail to intercept — a mechanism that covers
some deletions and silently misses others is not a candidate.

### 8.3 What made the backstop possible

| Operation | Cost | Coverage |
|---|---|---|
| `tmutil localsnapshot` (whole volume) | **180 ms, ~3 MB written** | every file on the volume |
| `clonefile` over the current default scope | 435 ms, ~3 MB | 3,314 files |
| `clonefile` over the whole home directory | **~183 s** (7,394 files/s) | 1,353,851 files |
| Metadata scan, whole home | 27 s (169,425 files/s) | — |
| Metadata scan, home minus `~/Library` and build caches | **5.6 s** | 340,595 files |

An APFS snapshot is an O(1) metadata operation: it does not copy data, so its cost is
independent of how many files the volume holds. **The whole-volume snapshot is both faster
than the current narrow clone and larger in coverage by three orders of magnitude.** The
worry that motivated a narrow scope — time spent making the agent wait, and write
amplification against SSD endurance — does not apply to it.

For scale: one cold `go build` in this repository writes 273 MB, about ninety times what a
snapshot writes.

### 8.4 Two costs that are not symmetric

Creating a snapshot needs no privileges. **Mounting one to read a file back requires root**
(`mount_apfs` returns `Operation not permitted` as an ordinary user; `fs_snapshot_create`
returns `EPERM`). Privilege is therefore needed only on the rare, deliberate restore path,
never on the hot path where the agent waits.

Local snapshots are also **purgeable**: `deleted(8)` reclaims them under disk pressure. On
the development machine they survived five days, but that is observed behaviour, not a
guarantee.

### 8.5 TCC changes what "scope" can mean

The Photos library is unreadable to an ordinary process: listing, reading and cloning
`~/Pictures/*.photoslibrary` all return `Operation not permitted`. **Adding `~/Pictures` to
the clone scope would capture nothing.**

Two consequences, and the second is reassuring:

1. `clonefile` and the metadata scan are both blind to TCC-protected data, so they must
   report those paths as outside coverage rather than reporting zero changes.
2. The agent is unring's child and shares its TCC context, so **whatever the agent can
   destroy, unring can capture, and whatever unring cannot read, the agent cannot delete.**
   The coverage gap is symmetric rather than one-sided.

A volume snapshot is a block-layer operation and is not subject to TCC, so protected data
is expected to be inside it. See §8.8 — this was not verified.

### 8.6 Decisions

| # | Decision |
|---|---|
| 13 | Take a **whole-volume APFS snapshot** at session start as the backstop; keep `clonefile` for the precise change list and for privilege-free restore |
| 14 | The **change list scans wider than the clone**: the home directory minus `~/Library`, `node_modules`, `.git`, `.cache` and `go/pkg` — 340,595 files, ~5.6 s |
| 15 | **Do not copy changed files out of the snapshot** at session end. Restoring from the snapshot requires `sudo`, and the purge risk is accepted rather than engineered around |
| 16 | The **default clone scope is unchanged**. A config file at the state directory adds paths; `--watch` becomes **additive** and `--watch-only` takes over today's replacing behaviour |
| 17 | A **default** path that does not exist is skipped silently. This decision originally allowed an explicitly named missing path to run after being **reported**; the end-to-end finding recorded in §8.9 overturned that half. A missing config `watch`, `--watch`, or `--watch-only` path is now a hard preflight refusal, recorded as `not_started`, and the child does not start |
| 18 | When the backstop is unavailable — no Time Machine, an excluded path, or Linux — **say so prominently and keep running**. Do not refuse to start |

Decision 16 changes `--watch` because the current semantics is the failure this project
treats as most serious. Someone who runs `--watch ~/Pictures` intending to add protection
**silently loses protection of their project tree** and is told the session succeeded. The
help text does document it; documented footguns still fire.

### 8.7 This revises decision 2

Decision 2 said the snapshot mechanism would be our own and that "Time Machine is an
analogy for the experience, not a dependency." **The backstop makes it a dependency.**
`tmutil localsnapshot` is the only unprivileged route to a volume snapshot, and its manual
page is explicit that it covers only "volumes included in the Time Machine backup."

This is a real reversal and is recorded rather than smoothed over. It does not extend to
the rest of the product: `clonefile`, the change list and restore remain ours, and
decision 18 exists precisely so that a machine without Time Machine degrades loudly instead
of appearing protected.

`tmutil isexcluded <path>` answers per path without privileges, which is what decision 18
needs to be honest rather than approximate.

### 8.8 What was not verified

- **That TCC-protected data is really inside the snapshot.** Reasoned from the snapshot
  being a block-layer operation; confirming it requires mounting one, which requires root.
- **Whether `tmutil localsnapshot` works with Time Machine unconfigured.** The development
  machine has it configured, so the negative case could not be produced there.
- **Linux.** It has neither `clonefile` nor an equivalent backstop, so its protection is a
  tier below macOS. The documentation must say so plainly.

### 8.9 Three more times measurement overturned reasoning

Continuing the list in §7:

4. "Broader coverage means cloning more." Cloning the whole home measured **183 seconds**,
   while a whole-volume snapshot covering strictly more measured **180 milliseconds**. The
   premise that coverage must be bought with time was simply false.
5. "A full-home metadata scan takes 7 seconds." Extrapolated from `~/go`, which is a
   shallow uniform tree that was already hot in the page cache. Measured on the real home
   directory: **27 seconds**, a fourfold error — and the same mistake as §7's item 3, made
   again on a different quantity.
6. "Reporting a missing explicitly watched path is enough." In a real session the warning
   used the same alarm prefix as routine Unix-socket limitations, the child ran, and the
   requested protection was absent. Reporting alone was indistinguishable from routine
   noise. Explicitly named missing paths now refuse startup and leave a `not_started` audit
   record naming the path and reason; missing default roots remain silent.

---

## 9. Storage hygiene and content equality (2026-08-12)

Real use found two metadata-layer failures: audit records accumulated without a bound, and
`ctime` correctly noticed a rewrite but could not distinguish genuinely new bytes from an
identical rewrite. APFS also showed why `du` is the wrong reclaim figure: deleting gigabytes
of apparent clone size can increase free space by zero because clone stores share blocks.

### 9.1 Decisions

| # | Decision |
|---|---|
| 19 | Retention uses **one oldest-first age-and-byte plan**: 14 days by default through `config.yaml`'s `retention_days`, the existing persisted 5 GiB measured-allocation cap, and an unconditional newest-session guard. Each session is selected at most once when either limit binds. Automatic expiry is announced and recorded. `unring prune` is a non-destructive preview unless `--confirm` is given. Reclaim output reports unring's measured snapshot accounting and explicitly does not equate clone references with immediately freed disk space. `unring log` shows 50 newest sessions by default, discloses truncation, and accepts `--all`. |
| 20 | Metadata remains the **fast change oracle**, including `ctime`, but clone-backed regular files are byte-compared when type, size, mode, and link count agree and the file is at most 8 MiB. The comparison streams data and suppresses a change only after conclusive equality. Larger files, paths without a clone, and read errors keep the safe false-positive result. Explicit restore streams the selected equal-size clone regardless of mtime or size, allowing original bytes restored by hand to return `already restored`; a genuine byte difference still refuses and writes the sidecar. |

The 8 MiB bound is deliberately about automatic work multiplied across every changed path.
A restore is an explicit operation on selected paths, so paying one streaming comparison is
worth avoiding a false conflict and redundant sidecar. Neither decision changes manifest
membership: paths and metadata are recorded exactly as before, and human rendering alone
quotes control characters such as newlines.
