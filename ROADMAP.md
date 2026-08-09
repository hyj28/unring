# Roadmap

Working checklist for the first release (target: 10–12 weeks). Derived from
[docs/PROJECT-BRIEF.md](docs/PROJECT-BRIEF.md) §5. Each milestone is meant to be a
self-contained, reviewable change that leaves the tree green.

Legend: `[ ]` todo · `[~]` in progress · `[x]` done

---

## M0 — Scaffold

- [x] Repo, MIT license, README, roadmap
- [x] Go module, `cmd/unring` entrypoint, `make`/task targets for fmt/vet/lint/test
- [x] CI (GitHub Actions): build + vet + test on macOS and Linux

## M1 — Postgres shared-transaction proxy

The zero-compromise demo. Validated in the brief as V1/V2; build it first.

- [x] M1.1 Wire protocol skeleton — listen, answer the client handshake ourselves
      (`AuthenticationOk` / `ParameterStatus` / `BackendKeyData` /
      `ReadyForQuery{TxStatus:'T'}`), relay the simple query protocol upstream
- [x] M1.2 One shared backend transaction across every client connection in a session;
      a client `Terminate` must **not** end the transaction
- [x] M1.3 Serialize individual backend exchanges across concurrent clients. An idle
      client-visible transaction does not retain the backend lease: other clients can
      run read-only queries, while concurrent writes and a second `BEGIN` fail with
      SQLSTATE `55P03` rather than waiting indefinitely for an application-level
      dependency cycle
- [x] M1.4 Session decision: `COMMIT` / `ROLLBACK`, plus crash-safe default to rollback
- [x] M1.5 Extended query protocol (`Parse`/`Bind`/`Describe`/`Execute`/`Sync`) —
      required by every ORM; rewrite prepared-statement names to avoid collisions
      between clients sharing one backend (see pgbouncer's known pitfalls)
- [x] M1.6 Escape hatch for statements that cannot run in a transaction block
      (`CREATE DATABASE`, `DROP DATABASE`, `VACUUM`, `CREATE INDEX CONCURRENTLY`,
      `ALTER SYSTEM`, `CHECKPOINT`) — classify as *needs approval*, run on a separate
      non-transactional connection, mark the session as no longer fully reversible.
      Lock-waiting maintenance against relations locked by the shared transaction is
      refused up front, including broad `VACUUM FULL`, `CLUSTER`, and `REINDEX`
      operations; commit or discard first, then run it outside unring
- [x] M1.7 Change summary — what the transaction actually did, for the review screen
- [x] M1.8 Map client `BEGIN`/`COMMIT`/`ROLLBACK` and named savepoints onto private
      savepoints inside the shared transaction. This preserves the single-decision
      guarantee while letting transaction-managing clients and ORMs run
- [x] M1.9 Count `TRUNCATE` effects authoritatively so sessions containing it can be
      reviewed and committed instead of being forced to discard

## M2 — Wrapper CLI

- [x] M2.1 `unring run -- <cmd>` — start proxies, spawn child, forward signals,
      propagate exit code
- [x] M2.2 Environment injection for the child only (`PGHOST`/`PGPORT`/`DATABASE_URL`,
      upper/lower-case HTTP and HTTPS proxy variables, `ALL_PROXY`, `FTP_PROXY`,
      `NODE_EXTRA_CA_CERTS`, `SSL_CERT_FILE`, `CURL_CA_BUNDLE`)
- [x] M2.3 `unring claude` and friends — thin aliases over `run`
- [x] M2.4 Read-only sessions exit silently: no prompt when nothing was written

## M3 — Review interface

- [x] M3.1 Non-interactive text summary + commit/discard prompt (unblocks end-to-end)
- [x] M3.2 Bubble Tea TUI: overall commit/discard, expandable per-item detail
      (diffs, affected rows, request bodies). **No partial commit** — by design
- [x] M3.3 Un-intercepted traffic gets its own, unmissable section

## M4 — Audit log

- [x] M4.1 Structured per-session log of what really happened
- [x] M4.2 `unring log` to inspect past sessions

## M5 — HTTPS proxy

- [x] M5.1 Local CA generation, stored per-user, injected into the child process only
- [x] M5.2 MITM proxy: interception, recording, forwarding, classification, and
      staging are implemented; proxy-aware plain HTTP fails closed and protocol
      upgrades are explicitly tunneled and reported
- [x] M5.3 CONNECT passthrough for hosts we cannot MITM, reported as un-intercepted

## M6 — Adapters

- [x] M6.1 YAML adapter schema + loader (match / tier / idempotency key / undo)
- [x] M6.2 CEL expression evaluation for conditional rules and idempotency keys
- [x] M6.3 GitHub adapter, written in the community format
- [x] M6.4 Slack adapter, written in the community format
- [x] M6.5 HTTP heuristics for unknown services, defaulting mutations to
      *needs approval*

## M7 — `gh` PATH shim

- [x] M7.1 Shim injected at the front of the child's `PATH` — captures structured
      intent, no certificate trust required (works around Go binaries ignoring
      `SSL_CERT_FILE` on macOS)
- [x] M7.2 Run output-dependent `gh` mutations only after explicit approval;
      never synthesize a successful CLI result

## M8 — Compensating undo

- [x] M8.1 Undo actions declared per adapter, executed on discard
- [x] M8.2 Slack `chat.delete`; document precisely what GitHub cannot undo

## M9 — Local file rollback

- [x] M9.1 Snapshot the project and narrow high-risk paths before the child starts,
      using APFS clones with an explicit per-entry/full-copy fallback
- [x] M9.2 Record created, modified, and deleted files with `ctime` in the scan oracle;
      report every path that could not be captured
- [x] M9.3 `unring restore` lists and restores individual paths, refuses post-session
      conflicts by default, and writes the snapshot version alongside
- [x] M9.4 Cap retained snapshot space and evict oldest sessions; expose current usage
- [x] M9.5 Make HTTPS adapters and the `gh` shim opt-in with `--outbound`

## M10 — Volume-snapshot backstop and configurable scope

Design: [docs/LOCAL-ROLLBACK-DESIGN.md §8](docs/LOCAL-ROLLBACK-DESIGN.md). Decisions 13–18.
The narrow clone scope leaves anything outside it uncaptured and unreported; a whole-volume
APFS snapshot costs 180 ms and covers the disk.

- [ ] M10.1 Take a whole-volume snapshot at session start as the backstop, keeping the
      `clonefile` capture for the precise change list and privilege-free restore
- [ ] M10.2 Widen the change-list scan past the clone scope — home minus `~/Library`,
      `node_modules`, `.git`, `.cache`, `go/pkg` — and report paths TCC or permissions
      made unreadable rather than reporting them as unchanged
- [ ] M10.3 Restore a path from the volume snapshot, stating that it needs `sudo` before
      asking for it, and failing clearly when the snapshot has been purged
- [ ] M10.4 Config file at the state directory with `watch` and `exclude` lists; `--watch`
      becomes additive and `--watch-only` takes over the replacing behaviour
- [ ] M10.5 Report backstop coverage honestly: no Time Machine, a path excluded from it
      (`tmutil isexcluded`), or Linux — say so prominently and keep running
- [ ] M10.6 Skip a nonexistent *default* path silently; report a nonexistent path the user
      named explicitly

---

## Explicitly out of scope for v1

FSEvents acceleration · lazy database `BEGIN` · MySQL · teams, approval flows,
multi-user · multi-agent concurrency control · web UI.

## Open questions

- Whether `discard` should hand feedback back to the agent for a retry
  (not in v1, but do not architect it out)
- Whether approved lock-conflicting maintenance should be deferred until the final
  decision, so discard omits it entirely; this better fits the promise but changes
  what the agent observes during the session
- Whether TCC-protected data (the Photos library) is genuinely inside a volume snapshot.
  Reasoned from the snapshot being a block-layer operation; verifying it needs root
- Whether `tmutil localsnapshot` works at all with Time Machine unconfigured. Its manual
  page covers only "volumes included in the Time Machine backup"
- What Linux should do without `clonefile` or any backstop equivalent. Its protection is a
  tier below macOS and the documentation has to say so rather than imply parity
