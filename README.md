# unring

> **Make everything your agent does undoable.**

> **Current scope:** local file rollback plus transactional PostgreSQL by default;
> GitHub and Slack through HTTPS adapters and the `gh` PATH shim are opt-in.

`unring` wraps an AI coding agent, snapshots the files it can change, and keeps database
writes reversible. File changes need no end-of-session decision: restore individual
paths later, when you have enough context to know something is wrong. PostgreSQL and
opt-in outbound effects retain their existing commit/discard review.

The name comes from *you can't unring a bell*. That is the whole point: now you can.

Outbound interception is deliberately off by default. A snapshot can give back what is
yours, but it cannot recall data already sent to another service; use `--outbound` when
that extra coverage is worth its prompts.

## Install

You need Go 1.26 or newer, cgo enabled, and a C compiler. The compiler is
required because `pg_query_go` embeds libpg_query, PostgreSQL's parser. Install
Xcode Command Line Tools or Clang on macOS, or GCC/Clang (typically the
`build-essential` package) on Linux, before running:

```sh
go install github.com/hyj28/unring/cmd/unring@latest
```

With the default Go configuration, that installs the binary at
`$(go env GOPATH)/bin/unring` (or at `$GOBIN/unring` when `GOBIN` is set). macOS
does not put the default Go bin directory on `PATH` automatically. Add it for the
current shell, then put the same export in your shell startup file:

```sh
export PATH="$(go env GOPATH)/bin:$PATH"
unring --version
```

A missing compiler fails during `go install`, before an `unring` binary exists;
check `go env CGO_ENABLED` (it must be `1`) and `cc --version` when diagnosing
that build-time error.

## First run

Wrap one bounded agent task: a run that does one job and exits so you can review one
coherent set of effects.

```sh
unring run -- claude -p 'Implement the requested validation change, run its tests, then stop'
# Or use any other agent's bounded/non-interactive command:
unring run -- your-one-shot-agent-command
```

Before the child starts, unring creates two independent layers of file protection. It asks
Time Machine for a whole-volume local APFS snapshot, then clones the project tree plus
`~/Documents`, `~/Desktop`, `~/.config`, `~/.ssh`, and `~/.aws`. A repeatable `--watch`
flag adds paths to that clone scope; use repeatable `--watch-only` when the named paths
should replace it. On APFS the clone fast path uses copy-on-write clones. If cloning is unavailable, unring
copies bytes for real and reports that cost. If one unreadable
path breaks the recursive clone, unring falls back to per-entry capture and names every
path it could not protect; an omitted path is never silently presented as snapshotted.
Symlinked watched roots are resolved, so iCloud-backed `~/Documents` and `~/Desktop`
remain protected while changes are reported using the paths the user watched. Nested
symlinked directory targets are not followed and are named as not snapshotted. Hard-linked
files are likewise named as outside coverage because per-path restore cannot preserve a
link group honestly.

The change list is wider than the clone scope: unring scans the home directory before and
after the child runs, excluding `~/Library`, `node_modules`, `.git`, `.cache`, and `~/go/pkg`.
The scan is metadata-only and its progress is announced because it can take several seconds.
If a watched-root walk cannot finish, that root is omitted from the diff and named as
unavailable; a partial walk is never interpreted as evidence that its unvisited files were
deleted.
A replacement `--watch-only` scope also replaces this wider scan; the clone diff already
covers exactly those explicitly selected roots. Changes elsewhere are not reported or
written to the audit record, even when the whole-volume snapshot contains them, so the
session can look clean after such a change. Additive `--watch` leaves the home-wide scan
enabled. Even then, the change list stops at the home scan and clone roots: changes in
locations such as `/etc`, `/opt`, `/tmp`, or another volume are not reported. Scan entry
counts and elapsed times are retained in the session audit record.
A change inside the clone scope remains restorable without privileges. A reported change
outside it is marked **snapshot only** and requires `sudo` to mount the read-only APFS
snapshot before restoring prior contents.

The optional `<state-root>/config.yaml` records additive watches and subtractive
exclusions. Config paths must be absolute or begin with `~/`; relative paths are rejected.
Exclusions also apply to `--watch-only`:

```yaml
watch:
  - ~/Pictures
exclude:
  - ~/Pictures/Lightroom Catalog
retention_days: 14
```

If an exclusion completely covers a path named by `--watch`, `--watch-only`, or the config
`watch` list, unring names that path as not snapshotted instead of silently dropping it.
Missing default directories are ignored because the user did not choose them. A missing
path named by `--watch`, `--watch-only`, or the config `watch` list is a hard preflight
error: the child does not start, and a `not_started` audit record names the path and reason.

Two limits are worth knowing before relying on the scan. Data protected by macOS privacy
controls — the Photos library, for instance — cannot be read by unring at all, so adding
it to the scope captures nothing; the same restriction applies to the agent unring runs,
which cannot delete what it cannot read either. And on Linux, where `clonefile` is
unavailable, capture copies bytes for real, so a wide scope costs real time and real
writes.

The whole-volume backstop is macOS-only and depends on Time Machine: `tmutil localsnapshot`
snapshots APFS volumes included in the Time Machine backup. If Time Machine is not
configured, a watched path is excluded from it, the filesystem is not APFS, or the platform
is not macOS, unring prints a prominent coverage warning and still runs. The clone layer and
change list continue to work. Local snapshots are purgeable under disk pressure; `unring
snapshots` reports whether each recorded backstop still exists.

The post-child Time Machine inclusion check passes changed paths to `tmutil isexcluded` in
validated batches instead of starting one process per path. Every returned line must name
the corresponding requested path in the same order; short, reordered, or malformed output
is rejected rather than attributed to the wrong file. Echoed paths are compared after Unicode
normalization because macOS may return decomposed filenames. Paths whose whitespace or line
breaks make a multi-path response unsafe are checked alone; any unusable batch falls back to
one check and one independently attributed result per path. The parser consumes tmutil's
two-space status separator without trimming the echoed path, so a path's own leading or
trailing spaces remain significant. The terminal reports scan and batch progress.

`SIGINT`, `SIGTERM`, `SIGHUP`, and a closed output pipe remain handled from the pre-session filesystem
scan and automatic retention, through the child, post-session scanning, interceptor sealing,
and finalization. A signal cancels the active phase, selects discard, and records an abnormal
outcome that later `log` and `restore` commands can inspect. Automatic retention reports
periodic progress even when it runs before the child. A second signal is acknowledged but does
not bypass durable discard finalization; the first signal determines the exit status.
When the first signal arrives during the child, the evidence-producing post-session filesystem
scan starts with a fresh cancellation context; a signal arriving during that scan still stops it.
Interrupted human-readable listings say that coverage is
incomplete without calling a canceled post-session walk a snapshot failure, while `log
--json` retains the exact diagnostic cause.

After the child exits, inspect and restore file changes at any later time:

```sh
unring restore <session-id>                  # list created, modified, deleted paths
unring restore <session-id> path/to/file     # restore selected paths
unring restore --all <session-id>            # restore covered paths except agent own-state
unring restore --all --include-agent-state <session-id>
unring restore --force <session-id> path     # explicitly overwrite a conflict
unring snapshots                             # inspect clone usage and APFS backstop presence
unring snapshots --all                       # include audit-only sessions with no restore data
unring prune                                 # show sessions outside retention limits
unring prune --all                           # name the complete retention set when over 50 sessions
unring prune --confirm <preview-token>       # remove exactly the previewed sessions
```

The `unring restore` and detailed `unring log <session-id>` output repeat the session's
recorded change-list scope. A clean stored change list therefore does not hide whether
`--watch-only`, a failed widened scan, or the normal home-and-clone boundary left changes
elsewhere unreported. A completed no-change scan prints an explicit zero. Human change rows
are bounded independently for each watched root, each declared agent-state root, and each
top-level outside-watch presentation root, so one noisy root cannot hide another;
`restore <session-id>` remains the complete recorded listing. The history outcome labels
distinguish a successful file-only session that needed no decision from an explicit discard
and an abnormal interrupted end. A reviewable session discarded by the documented
non-interactive default has its own `default discard` label; it is not reported as abnormal.

A clone-covered path changed after the session is refused by default. Its pre-session
snapshot is written alongside the current file, and only `--force` permits replacement.
Snapshot-only restore announces why root is required before invoking `sudo`; it fails
clearly if Time Machine has already purged the recorded APFS snapshot. Coverage
gaps created during a session are named in the change list and audit record; they do not
prevent restoring independently covered paths. Snapshot retention defaults to 5 GiB of
measured snapshot allocation, and stored sessions expire after 14 days unless
`retention_days` in `config.yaml` selects another positive number. The age and byte limits
share one oldest-first decision, and the newest session is always kept. Automatic expiry is
named in the new session's output and audit record. `unring prune` shows the same retention
set without deleting it; the printed preview token is valid for 24 hours and is required to
remove exactly that saved set. Confirmation recomputes current eligibility under the
retention lock and refuses the token if the newest session, limits, or stored state changed.
Expired and abandoned tokens are collected by later prune invocations. Reported
snapshot bytes are unring's retention accounting, not a promise of immediately increased
free space: APFS clones can release references to shared blocks without changing free space
until the other references are removed.
Automatic retention groups removed session IDs by reason and states the accounting caveat
once for the shown set; its total, withheld count, detail command, and complete recorded
retention events remain unchanged.
Age expiry removes both the stored session record and clone restore data. When only the byte
cap binds, unring removes the clone data but keeps the audit record of database, outbound,
and file activity, marking that record as no longer retained for clone restore.
An explicit `--snapshot-cap-bytes` value is persisted so later runs and `unring snapshots`
report and enforce the same cap. `UNRING_SNAPSHOT_CAP_BYTES` supplies the initial cap for a
state directory that has no persisted value; the first run persists that effective value.
Where a filesystem cannot expose allocation changes cheaply, unring labels the figure as
an upper bound and does not evict snapshots based on that estimate.

Human-readable `unring log` lists at most the newest 50 sessions by default and says when
older records were omitted; use `unring log --all` to request every human-readable row.
`unring log --json` always returns every record so redirected structured output is complete.
`unring snapshots` uses the same 50-session bound for sessions with retained or possibly
present restore data, summarises audit-only sessions whose clone and volume snapshots are
gone, and uses `--all` to show every audit record with an explicit restore-data status.
`unring prune` also bounds its default per-session listing at 50. When the retention set is
larger, the bounded listing deliberately issues no confirmation token; `unring prune --all`
names the complete set before issuing the token.

Change detection keeps metadata as its fast oracle. For a clone-backed regular file whose
type, size, mode, and link count still agree, unring compares bytes only up to 8 MiB before
calling a metadata-only difference a modification. Larger files, files outside clone
coverage, and comparison errors remain reported because unring could not establish
equality. An explicit restore is different: when the clone is available, it streams the
selected equal-size file once regardless of mtime, so manually returning to the original
bytes is recognized as `already restored` without weakening the conflict guard. A
volume-only equal-size conflict requires the already-announced privileged snapshot mount
before unring can truthfully report either `already restored` or a byte-level conflict.

Snapshot-only paths from the same recorded APFS snapshot are restored under one read-only
mount per command, with one unmount after every selected path has been attempted. The
declared agent-own-state roots are `~/.claude`, `~/.codex`, `~/.cursor`, `~/.config/opencode`,
`~/.local/share/opencode`, and `~/.cache/opencode`. Their changes remain in live output,
the manifest, and stored listings under a separate label. Their logical and physically
resolved roots are persisted
with the session, so later restores do not depend on the restoring process's `HOME`.
`restore --all` names and skips
them by default because rolling back an active agent's transcript or session state can be
harmful; restore one explicitly by path or use `--include-agent-state` with `--all`.
Unsupported special files such as Unix sockets also remain recorded, but render as an
informational file-type note rather than as an actionable `FILE NOT SNAPSHOTTED` alarm.
Records created before agent-state roots were stored disclose that their grouping is inferred
from the current environment whenever that grouping is listed or used by `restore --all`.
Live and stored session reviews show at most 50 ordinary changes and 50 agent-own-state
changes, disclose each group's withheld count, and point to `unring restore <session-id>`
for the complete recorded list. Automatic retention uses the same 50-item human-output
bound, announces the total and withheld count, and records every removal in the current
session even when its detail is not printed.

If this task needs PostgreSQL coverage, point `DATABASE_URL` at the real development
database first:

```sh
export DATABASE_URL='postgresql://user:password@real-host/database'
unring run -- your-one-shot-agent-command
```

Keep database-backed runs bounded. The shared PostgreSQL transaction remains open for
the wrapped command's entire lifetime. An open-ended `unring claude` session can
therefore hold locks for hours, block DDL, prevent autovacuum cleanup, and make
concurrent clients fail with SQLSTATE `55P03`. One final commit/discard decision also
applies to the whole run, with no partial commit, so a fifteen-task interactive session
is the wrong review granularity.

PostgreSQL is optional. Precisely, unring considers it **not configured** when
`DATABASE_URL` is absent, empty, or contains only whitespace. In that mode it does not
start the PostgreSQL proxy or inject PostgreSQL connection settings, but file snapshots
and the audit log still run. The review explicitly says
that no database traffic was intercepted; that statement is not evidence that the
child did not access a database through inherited `PG*` variables, command arguments,
a service file, or another client-specific setting.

Any nonblank `DATABASE_URL` means PostgreSQL **is configured**. The URL must parse and
the database must be reachable, authenticated, and supported or unring exits before
starting the child. A configured-but-unreachable database is never silently downgraded
to no-database mode. Run `unring --help` for the command forms and safe non-interactive
defaults.

PostgreSQL 14 is the minimum supported version. Older servers are rejected at startup
with an explicit version error before any client traffic is accepted; CI exercises
the integration suite against PostgreSQL 14 and 17 explicitly.

When PostgreSQL is configured, `unring` opens one real transaction and binds the
Postgres proxy to an ephemeral loopback port. It injects the connection
variables into the child process only. Every Postgres client connection opened by that
child uses the same backend transaction; individual protocol exchanges are serialized
because PostgreSQL has only one backend connection. Closing one client connection does
not close the transaction.

Pass `--outbound` to start HTTPS interception, adapters, and the `gh` shim:

```sh
unring run --outbound -- your-one-shot-agent-command
```

With that flag, the child receives upper- and lower-case HTTP/HTTPS proxy
variables, plus `ALL_PROXY` and `FTP_PROXY`. Existing
`NODE_EXTRA_CA_CERTS`, `SSL_CERT_FILE`, and `CURL_CA_BUNDLE` files are merged with
unring's local CA rather than discarded. Node.js, curl, and Python's standard library
therefore trust both their prior roots and unring's CA without changing trust for the
user's shell or any other process. If the parent environment requires an upstream
HTTP or HTTPS proxy, unring honors it when forwarding and for CONNECT passthrough.

Intercepted HTTPS requests are classified by ordinary YAML adapters, then conservative
HTTP heuristics. Stageable calls do not reach their origin: the client receives an
explicitly marked synthetic response, and the review shows the call under
**PENDING HTTPS — WILL BE SENT IF YOU COMMIT**. Needs-approval calls stop while unring
asks; a decline guarantees the request is not sent. Safe-method requests and approved
calls that really ran are shown separately from traffic unring could not intercept.
Approvals must be answered interactively: input queued before the prompt is deliberately
discarded so a keystroke intended for the child cannot approve an action, and without a
terminal unring safely declines rather than accepting pre-supplied input.
Proxy-aware plain HTTP remains blocked and reported rather than escaping invisibly.
The complete community adapter format is documented in
[docs/ADAPTERS.md](docs/ADAPTERS.md).

An agent must be able to reach its own model API and send its own operational
telemetry without turning approval into noise. For the three named agent commands,
unring therefore forwards only these enumerated control-plane requests without
approval:

| Wrapped command | Deliberately ungated operational requests |
|---|---|
| `claude` | `POST https://api.anthropic.com/v1/messages`, `/api/event_logging/v2/batch`, or one `/api/eval/<event-id>` endpoint; `POST /api/v2/logs` on `http-intake.logs.us5.datadoghq.com` or `browser-intake-us5-datadoghq.com` |
| `codex` | `POST https://api.openai.com/v1/responses`; `POST` or WebSocket `GET` to `https://chatgpt.com/backend-api/codex/responses`; `POST https://ab.chatgpt.com/otlp/v1/metrics` |
| `opencode` | `POST` to `/zen/v1/responses` or `/zen/v1/chat/completions` on `opencode.ai`, `/v1/messages` on `api.anthropic.com`, and `/v1/responses` or `/v1/chat/completions` on `api.openai.com` |

The match is enabled only when the wrapped executable's basename is the listed agent,
and it is exact on method, hostname, and endpoint shape. The Claude evaluation rule
accepts exactly one non-empty path segment in place of `<event-id>`; it is not a path
prefix. Custom providers, gateways, other paths on those hosts, and genuinely
unknown services still use the normal fail-closed
classification. Because the proxy cannot distinguish an agent's own request from a
descendant process using the same endpoint, the exemption applies to the whole wrapped
process tree for those exact requests. Every match is labeled **AGENT CONTROL PLANE —
FORWARDED WITHOUT GATING** in the JSON audit and in any session review that is otherwise
needed. Control-plane calls alone do not manufacture a commit/discard prompt.

With `--outbound`, `gh` is handled without TLS interception. For each run, unring creates a private
directory, places a `gh` shim there, and prepends that directory only to the wrapped
child's `PATH`. Enumerated reads such as `gh --version`, `gh auth status`,
`gh issue list`, `gh pr view`, and a method-safe `gh api` GET execute the real pre-resolved
`gh` with unchanged standard streams, exit status, stdin, and terminal. Output-dependent
mutations such as `gh issue create` require approval and cannot be staged honestly:
declining returns non-zero with no stdout, while approval runs real `gh` immediately
so callers receive its real URL and status. Unknown subcommands or flags stop for
approval with the exact ambiguity rather than being guessed. The directory
and socket are removed when the session ends; no shell profile or persistent `PATH`
is changed.

After the child exits, unring records file changes without asking for a file decision.
If PostgreSQL or opt-in outbound effects require review, it separately asks whether to
commit or discard them. Without a configured database, the review instead says plainly
that PostgreSQL was not intercepted. Automation must choose explicitly when it has
reviewable database or outbound effects:

```sh
unring run --commit -- your-command
unring run --discard -- your-command
```

Without a terminal or a decision flag, the session defaults to discard. SIGINT,
SIGTERM, an unring panic, or loss of the real database connection also defaults to
rollback. The child's exit code is returned after a successful decision.
If an interactive child is stopped with Ctrl-Z, unring reclaims the terminal,
terminates the stopped child, and discards the session; nested job suspension is not
preserved because an arbitrary parent process cannot be trusted to resume it safely.

Database integration tests start and stop their own throwaway PostgreSQL cluster.
They skip when PostgreSQL is not installed; CI and explicit verification require a
real server and fail instead of skipping:

```sh
make test-integration
```

## Audit log

Every run creates a structured JSON audit record before the child starts and updates
it atomically as the session progresses. It includes start and end times, the requested
decision and confirmed outcome, watched paths, uncaptured paths, created/modified/deleted
files and restore outcomes, per-table row changes, schema changes, irreversible actions
approved or declined, intercepted HTTPS requests, and anything unring saw but could not
intercept. It also records whether outbound interception was enabled and whether PostgreSQL was active or not
configured and the fixed structural blind-spot disclosure. Signal termination, a
recoverable unring panic, and backend loss all retain a record; an unknown database
outcome is recorded as `unknown`, never as a successful discard.

Audit records intentionally contain the child's complete argument vector, full
PostgreSQL statement text, argument vectors for mutating or ambiguous `gh` calls,
and complete request and compensation URLs, including query strings. They also
store command tags and errors, status codes, classifications, approval decisions,
idempotency keys, and compensation outcomes. Those fields can contain tokens,
SQL literals, message text passed as an argument, or other secrets, so protect the
state directory accordingly. Unring does not store HTTP headers, request or
response bodies, cookies, authorization headers, database environment variables,
or CA key material as separate audit fields; a secret supplied in a command
argument, SQL statement, or URL is necessarily retained there. `unring log` skips
a damaged or unsupported record, prints a warning naming it, and still lists the
readable history.

```sh
unring log                    # list past sessions, newest first
unring log <session-id>       # human-readable detail; an unambiguous prefix works
unring log --json <session-id>
unring restore <session-id>
unring snapshots
```

The per-user state root is `$XDG_STATE_HOME/unring` when `XDG_STATE_HOME` is set,
otherwise the platform user-config directory plus `unring` (on macOS,
`~/Library/Application Support/unring`). `UNRING_STATE_DIR` is an explicit override
for isolated installations and tests.

Clone snapshots and metadata baselines are stored beneath
`<state-root>/snapshots/<session-id>`; unring excludes its own state root if it is inside a
watched project tree. Whole-volume snapshots remain APFS-managed and are named in the
session audit record rather than copied into unring's state directory.

The CA certificate is stored at `<state-root>/ca/ca.pem`; its private key is
`<state-root>/ca/ca-key.pem`, inside a mode-`0700` directory with mode-`0600`
permissions. The CA is generated once and reused. The private key is never injected or
logged—only the certificate path is passed to the child. Unring never installs the CA
in the system trust store or macOS keychain and never modifies a shell profile.
Keeping it in private per-user state gives wrapped children a stable CA across runs
without broadening trust for any process unring did not start. When inherited CA bundle
variables must be preserved, unring writes a mode-`0600` merged public-certificate
bundle beside its CA files; it never copies a private key into that bundle.

## How it works

Three layers, one promise:

| Layer | Mechanism | Reversibility |
|---|---|---|
| Database | A real transaction. `discard` is a `ROLLBACK`. | Fully reversible |
| Stageable external calls | Never sent, only recorded. | Fully reversible — it never happened |
| Calls that must really run | Approved up front, compensated afterwards | Best effort, with stated limits |

Your agent is not running against a simulation. Database statements really execute,
inside a transaction the agent shares across every connection it opens — so it reads
back its own writes, gets real results, and real errors. What it does not get is the
ability to make any of it permanent without you saying so.

One commit/discard decision always applies to the whole session; partial commit is not
available. Keeping database changes while withholding one related external action
would create states that are difficult to reason about—for example, committing
`notified_at` while the corresponding mail was never sent.

## Design commitments

- **Silently failing to intercept something is this project's worst failure mode.**
  Traffic we cannot intercept is reported as un-intercepted, loudly, in the review
  screen. We would rather tell you "this part got past us" than let you believe you
  were covered.
- **No LLM in the classification path.** Known services are matched by declarative
  adapters, unknown ones fall back to HTTP heuristics, and anything uncertain stops
  and asks you. A classifier that is usually right is not good enough here.
- **Built-in adapters use the exact same format as community ones.** No privileged
  path for the adapters we ship — that is how plugin formats rot.
- **This guards against accidents, not against a hostile agent.** An agent that
  deliberately routes around the proxy can.

## Honest limits

- Every review includes a short fixed warning for channels that never reach an unring
  interception point and therefore leave no session record: SSH traffic (including
  `git push` over SSH), direct-to-IP connections, raw sockets, and clients that ignore
  proxy or PATH settings. On macOS that includes unshimmed Go CLIs such as `aws`,
  `docker`, `terraform`, and `kubectl`. The warning is structural, not evidence that
  any of those channels were used in a particular run.
- Compensating undo is a documented-limits fallback, not the main guarantee. The
  strongest outcome is a staged call that was discarded and therefore never reached
  its service. For a call that really ran, review says before the decision whether
  discard has a declared compensation, what it will attempt, and what may remain.
  Unring durably records the attempt and never reports success from an HTTP error,
  transport error, or a Slack `ok: false` response. A failed or impossible
  compensation makes the session outcome unknown and prominently names the surviving
  effect.
- Slack `chat.delete` can delete a message posted by the same bot token. It cannot
  make a message unseen: someone may already have read or copied it, and permission
  or service failures can leave it posted.
- GitHub's REST API cannot delete an issue. Unring's declared compensation closes a
  created issue; the issue and its history remain visible in the closed state. GitHub
  has a GraphQL `deleteIssue` mutation, but it requires administrator permission, so
  unring does not present that as generally available undo.
- Delivered mail cannot be recalled reliably. Calls with no declared compensation
  are labeled as effects discard cannot undo.
- A final commit replays staged HTTPS calls before committing the Postgres
  transaction. If replay fails, unring rolls the database back and reports an unknown
  overall outcome because an earlier staged call may already have reached its origin.
  Declared idempotency keys make service-side retry protection possible, but there is
  no distributed transaction across an HTTP service and Postgres.
- Go binaries on macOS do not honor `SSL_CERT_FILE`; Go's `crypto/x509` uses the
  system keychain. Unring deliberately does not install its CA there. `gh` is covered
  by the per-session PATH shim described above. Other Go clients may fail the MITM
  handshake or require explicit CONNECT passthrough and are reported as
  un-intercepted; this is not silent coverage.
- A host can be deliberately passed through with
  `UNRING_HTTPS_PASSTHROUGH=host1,host2`. The CONNECT tunnel still uses the loopback
  proxy, but unring cannot see its requests or bodies; the host is therefore shown
  prominently as un-intercepted in both review and audit. Passthrough can chain
  through inherited HTTP and HTTPS upstream proxies; unsupported upstream schemes
  fail visibly and are recorded.
- HTTP protocol upgrades such as WebSockets are tunneled bidirectionally after the
  HTTPS handshake. Their host and the fact that payloads were not inspected are shown
  as un-intercepted in review and audit.
- Coverage depends on clients honoring standard proxy variables. Proxy-aware plain
  HTTP is blocked and reported; proxy-aware HTTPS is intercepted or reported. A child
  that overrides proxy settings, uses a tool-specific bypass, speaks a protocol that
  ignores these variables, or opens a direct socket can evade interception entirely.
  Unring routes common HTTP/HTTPS/FTP/ALL proxy variables to loopback and clears
  inherited `NO_PROXY` to close accidental exclusions, but it is not a hostile-process
  network sandbox.
- A configured database keeps one real transaction open for the entire wrapped
  command. Long-lived interactive sessions can retain locks, block DDL, delay
  autovacuum cleanup, and cause concurrent unring clients to fail with SQLSTATE
  `55P03`. Prefer one bounded agent task per unring run; one decision applies to the
  whole run and partial commit is intentionally unavailable.
- PostgreSQL `nextval` calls do not roll back, including values consumed by
  identity/serial inserts, so discarded runs can leave ID gaps. The sequence reset
  performed by `TRUNCATE ... RESTART IDENTITY` is a PostgreSQL exception: that reset
  is transactional and does roll back with the truncation.
- PostgreSQL does not expose authoritative transaction counters for `TRUNCATE`, so
  unring takes `ACCESS EXCLUSIVE` locks over the complete truncate set and runs an
  exact `COUNT(*)` on every physical table before forwarding the statement. This
  covers multiple targets, recursive foreign-key `CASCADE`, and partitioned tables;
  partitioned parents are reported per leaf partition.
- That exactness has a visible cost: a normally fast `TRUNCATE` now scans every
  affected table and can take as long as a full table scan. unring sends the client a
  PostgreSQL notice before counting; there is no approximate fast mode. Acquiring the
  required `ACCESS EXCLUSIVE` locks can also wait indefinitely behind another database
  session (even an idle transaction that has read one affected table). While it waits,
  the shared backend is occupied, so every other client connected to that unring session
  waits too; PostgreSQL's normal lock-wait behavior and cancellation apply.
- Exact TRUNCATE accounting applies when `TRUNCATE` is the parsed top-level statement.
  A `DO` block, procedure call, or volatile function call can execute SQL hidden inside
  server-side code; unring cannot inspect that body and therefore reports the successful
  statement as `UNKNOWN` instead of claiming that zero rows changed. If an exact
  count cannot be proven — for example, a foreign table is involved, row-level
  security hides rows, the role cannot run `COUNT(*)`, or an enabled `ON TRUNCATE`
  trigger could change the effect — the successful statement remains explicitly
  `UNKNOWN` and the session is forced to discard.
- Postgres only. MySQL commits DDL implicitly, which breaks the core guarantee.
- Both PostgreSQL's simple and extended query protocols are supported. Prepared
  statement and portal names are isolated per client on the shared backend.
- Statements that must run outside a transaction (`CREATE DATABASE`, `VACUUM`,
  `CREATE INDEX CONCURRENTLY`, `ALTER SYSTEM`, `CHECKPOINT`, and similar) require
  approval and make the session not fully reversible. They are refused if the shared
  transaction already has uncommitted database changes.
- Lock-waiting maintenance commands cannot run against a table while the shared
  transaction holds locks on it. This includes concurrent index builds, `VACUUM FULL`,
  `CLUSTER`, and `REINDEX`, including their database-wide or schema-wide forms. Unring
  checks both concrete and broad targets before execution and reserves the shared
  backend through the escape operation; commit or discard the session first, then run
  the maintenance command separately. A short lock timeout remains as a backstop for
  conflicts outside the session that cannot be predicted.
- Client transaction control is mapped to private savepoints. One client-visible
  transaction may be open at a time; it does not pin the backend while idle.
- The shared transaction uses PostgreSQL's default `READ COMMITTED` isolation. Its
  catalog baseline is captured explicitly; this also lets review see approved DDL
  committed on the non-transactional connection.
- While that transaction is open, other clients may run read-only queries. Unring
  rolls those query cycles back internally so they cannot become part of the open
  client's savepoint. A concurrent write or second `BEGIN` fails immediately with
  SQLSTATE `55P03`: waiting would recreate the cross-connection deadlock this policy
  is intended to prevent.
- Connection options passed directly as child command arguments can bypass injected
  environment variables. This tool guards against accidents, not deliberate bypass.
- Discard rolls back the shared PostgreSQL transaction and omits staged external
  calls. It does not revert filesystem changes (use git), statements already approved
  to run outside the transaction, or already-forwarded external effects. For a
  forwarded effect it only attempts the adapter's declared compensation, with the
  Slack, GitHub, mail, and partial-failure boundaries above.
- Some effects genuinely cannot be undone. The value is that most side effects never
  happen at all; compensation is only the fallback.

## License

MIT
