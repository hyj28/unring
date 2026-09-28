# Changelog

All notable changes to unring are documented here. Releases follow
[Semantic Versioning](https://semver.org/spec/v2.0.0.html) while the project is pre-1.0.

## [Unreleased]

### Removed

- The built-in Slack adapter. Message recall is not a capability this project
  carries for now. The staging and compensating-undo machinery is unchanged and
  still exercised, through a test-local stageable adapter rather than a built-in
  one: no shipped adapter declares the stageable tier any more.

## [0.3.0] - 2026-09-28

### Added

- Naming a path recovers its recorded baseline from a retained clone when an
  interrupted post-session scan left the change list empty, with conflict
  protection, a preserved `.snapshot` sidecar, and an explicit `--force`.
- Release builds can be told their version through ldflags; `make install` and
  `make dist` derive it from `git describe`, so an unclean tree keeps its
  `-dirty` marker.

### Changed

- The session summary and the human `unring log` now state that the
  commit/discard decision does not revert files, and that reverting them is a
  separate explicit step. `unring log --json` is unchanged.
- A coverage gap consisting only of the permanent macOS permission refusals is no
  longer announced as an abnormal session by `unring restore`.
- Agent own-state groups larger than ten collapse to a per-root count in the
  end-of-session summary; the explicit `unring restore <id>` listing stays
  expanded.
- `restore --all` refuses only when there is no observed change list to work
  from, and one unrecognised path no longer aborts a whole batch.

### Fixed

- Restore no longer exits 0 without mentioning a path the user named.
- An unsealed snapshot is no longer reported as evicted; absent, unsealed,
  unknown and retained are distinct statements.
- Retention no longer claims it evicted the oldest snapshot when cap eviction
  skipped a store whose byte accounting is not exact.

## [0.2.0] - 2026-08-27

### Added

- Local file rollback with per-path restore, APFS clone snapshots, and a macOS
  Time Machine local-snapshot backstop.
- Configurable additive and replacement watch scopes, explicit exclusions, and
  persistent coverage-gap reporting.
- Snapshot retention by age and measured allocation, plus token-confirmed pruning.
- Durable session audit records, bounded history views, and complete JSON output.

### Changed

- Outbound HTTPS interception and the `gh` shim are now explicit opt-in behavior
  through `--outbound`.
- Session finalization survives interrupts and closed output pipes while retaining
  the evidence collected so far.
- File equality, conflict detection, restore status, and macOS permission reporting
  are more accurate and conservative.

## [0.1.0] - 2026-07-30

The first tagged release: transactional PostgreSQL interception, review and
commit/discard, GitHub and Slack adapters, the `gh` PATH shim, compensating undo,
and structured audit logging.

[Unreleased]: https://github.com/hyj28/unring/compare/v0.3.0...HEAD
[0.3.0]: https://github.com/hyj28/unring/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/hyj28/unring/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/hyj28/unring/releases/tag/v0.1.0
