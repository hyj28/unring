# Security policy

unring sits on a sensitive boundary: it observes filesystem changes, database
statements, and opt-in outbound requests made by another process. Please do not
open a public issue for a vulnerability that could cause silent loss of coverage,
credential exposure, or unintended side effects.

## Reporting a vulnerability

Use [GitHub's private vulnerability reporting](https://github.com/hyj28/unring/security/advisories/new).
Include the affected platform and unring version, the smallest reproduction you
can share safely, the coverage or trust boundary you expected, and the behavior
you observed.

You should receive an acknowledgement within seven days. Please allow time for a
fix and coordinated release before publishing details.

## Supported versions

Security fixes are made on the latest tagged release and on `main`. Older releases
may receive a fix when the change can be backported safely, but this is not guaranteed.

## Scope

Reports about silent interception failures, restore corruption, unintended secret
retention, proxy trust expansion, and incorrect commit/discard outcomes are especially
valuable. unring is designed to guard against accidental side effects; deliberate
sandbox escape by a hostile child process is outside the current threat model unless
the documentation claims that channel is covered.
