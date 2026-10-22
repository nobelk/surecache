# surecache

**Sure, or it asks.**

`surecache` is an in-process, read-mostly Go cache for systems where a wrong answer is
worse than a slow one. It sits in front of an authoritative store, answers hot reads from
memory only when it can prove the answer matches the store's committed state, and
otherwise defers so the caller asks the store. Writes are always synchronous to the
store; the cache never expires, evicts or serves stale data on error.

> **Status: pre-release.** The API is unstable and may change without notice before v1.0.0.

- Design and guarantees: [docs/design.md](docs/design.md)
- Implementation plan: [docs/plan/README.md](docs/plan/README.md)
- Contributing: [CONTRIBUTING.md](CONTRIBUTING.md)

## License

Apache-2.0. See [LICENSE](LICENSE).
