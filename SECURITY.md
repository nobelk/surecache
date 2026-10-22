# Security policy

`surecache` is meant for systems where a wrong answer is worse than a slow one. A bug
that makes the cache answer when it should defer (a stale or wrong `Found` or `Absent`)
is treated as a security issue, not only as a correctness issue.

## Reporting a vulnerability

Please report vulnerabilities privately through GitHub security advisories:
<https://github.com/nobelk/surecache/security/advisories/new>.

Do not open a public issue for a suspected vulnerability. Include the version, the Go
version, the store, the outage policy and, if possible, a failing test or a minimal
reproduction.

You can expect an acknowledgement within a week. Fixes are released as patch versions
and announced in the advisory and in `CHANGELOG.md`.

## Supported versions

Before v1.0.0 only the latest minor release receives fixes.
