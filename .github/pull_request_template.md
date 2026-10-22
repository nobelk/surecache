## Summary

<!-- What changed and why. Link the issue or plan milestone. -->

## Requirements and invariants

<!-- Which requirements (SAFE-n, FUN-n, NFR-n) does this cover? If it touches the lock,
the marks, the generation or the indexes, list the section 10.4 invariants it affects and
the tests that show they hold. -->

## Checklist

- [ ] Tests written first; each names the requirement it covers
- [ ] `make lint` and `make test` pass
- [ ] `make cover` passes
- [ ] No store, log or observer call under the cache lock
- [ ] Every exported identifier has a doc comment
- [ ] `CHANGELOG.md` updated under "Unreleased"
