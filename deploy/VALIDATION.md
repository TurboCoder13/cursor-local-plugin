# Deployment validation

Checked 2026-10-04 against the CPA release pinned in `runtime.json`.

## Native loading

- macOS arm64: native build, format, vet, race tests and isolated CPA host check
  passed. Provider coverage: 80.8%; login helper coverage: 28.7%.
- Ubuntu 24.04 amd64: compiled under Linux amd64, then loaded into the official
  checksum-verified CPA runtime in an Ubuntu container. Provider coverage: 80.6%;
  login helper coverage: 28.7%. Go race tests passed.
- Deployment tests passed on both systems: 8 tests, 52 assertions, including
  preservation of config, other plugins and credentials, wrong-architecture
  rejection, symlink rejection and refusal to overwrite an existing setup.
- The full lintro check reported zero issues for this provider repository.

The Ubuntu container ran through Intel emulation on the development Mac. This
validates Linux amd64 compilation and real CPA loading, including OAuth, quota
registration and authenticated management access. It does not validate the HP
laptop itself, systemd boot persistence, its network or power settings.
No account or inference request was used for these portability checks.

Windows and FreeBSD paths are implemented but have no live native acceptance
result. Run the isolated host check before deploying to another target.

## Subscription behavior

Earlier macOS live checks passed text, SSE and client tool handoff/result
continuation with `cursor/gpt-5.4-mini-none`. Installed quota retrieval and the
custom dashboard were also checked. Other protocol paths use mock coverage.
Those results do not prove every advertised model works or guarantee future
compatibility with Cursor's private API. Credentials and machine-specific
historical validation records remain outside Git.

GitHub Actions provide native Ubuntu and macOS build/loading checks. Their
results must be verified for each pushed revision; this document does not
predict or replace CI results.
