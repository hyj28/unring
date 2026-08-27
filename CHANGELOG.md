# Changelog

All notable changes to unring are documented here. Releases follow
[Semantic Versioning](https://semver.org/spec/v2.0.0.html) while the project is pre-1.0.

## [Unreleased]

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

[Unreleased]: https://github.com/hyj28/unring/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/hyj28/unring/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/hyj28/unring/releases/tag/v0.1.0
