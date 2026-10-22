# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this
project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html). The
`surecachepg` and `promobs` modules are versioned with their own tag prefixes.

## [Unreleased]

### Added

- Repository scaffold: root, `surecachepg` and `promobs` modules with a committed Go
  workspace, lint configuration, CI and nightly workflows, and open-source project files.
- Core types (`Spec`, `Source`, `Answer`, `RebuildResult`, sentinel errors), `Builder`
  with closed index registration and validation, options with budget defaults, the
  `Observer` interface with ordered state-event delivery, the capacity-bounded map, the
  build-aside bulk install and the `Get`, `Lookup` and `List` reads with their deferral
  rules.
