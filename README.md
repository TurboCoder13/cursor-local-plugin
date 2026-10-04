# Local Cursor provider for CLIProxyAPI

An independently written Cursor subscription provider. The same source builds
native libraries for CLIProxyAPI-supported operating systems. The management
HTML is portable; the native library must match the server OS and CPU.

No omsub implementation, tests, binary, protobuf descriptor, UI or updater is
included. See [PARITY.md](PARITY.md) and [REVIEW.md](REVIEW.md) for provenance,
behavior and implementation differences.

## Build and check

Install Go at the version required by `go.mod` or newer, a native C compiler,
Make, Ruby with YAML and Minitest, and GitHub CLI for the optional runtime fetch.
On Ubuntu, install `build-essential make ruby ruby-minitest git gh`; Ubuntu's
packaged Go may be too old, so check `go version` against `go.mod`.

```sh
make build
make check
ruby tools/fetch-runtime.rb
make host-check ARGS="--binary $PWD/build/runtime/cliproxyapi"
```

Artifacts go to `build/<os>/<arch>/`. Linux uses `.so`, macOS `.dylib`, and
Windows `.dll`. Windows requires a native CGO toolchain and a POSIX Make shell
such as MSYS2. Cross-compilation additionally requires the destination's C
compiler; changing `GOOS` alone is insufficient. Native compilation on the
server avoids this requirement.

The isolated host check uses an empty temporary credential directory and a
loopback port. It checks real CPA loading, OAuth, quota registration and
management authentication without inference or account access. Use `--port`
if 18317 is occupied. The pinned runtime release is in `deploy/runtime.json`;
its published checksum is verified before extraction. Do not use CPA's
`no-plugin` release variants.

Linux x86-64 on Ubuntu and Apple Silicon macOS are the acceptance targets.
Other loader-supported combinations require their own native host check;
source portability is not a claim that every OS/architecture has been tested.

## Install

For a new server, generate fresh local keys and a v8 configuration:

```sh
ruby tools/setup.rb
ruby tools/install.rb
ruby tools/install-dashboard.rb --dashboard-source /path/to/dashboard/dist/index.html
```

Setup refuses to overwrite existing configuration or keys. Provider credentials
must be signed in or transferred separately. Credentials, real configurations,
keys, logs and compiled artifacts do not belong in Git.

Every deployment path can be overridden. Existing installations should pass
`--config` rather than running setup:

```sh
ruby tools/install.rb --config /path/to/cliproxyapi.conf --binary /path/to/cliproxyapi
ruby tools/install-dashboard.rb --config /path/to/cliproxyapi.conf --dashboard-source /path/to/dist/index.html
```

The installer preserves other configured plugins and every configuration value
outside its plugin settings, verifies library architecture, and backs up files
before replacing them. It never writes account credentials. Restart CPA after
changing a loaded native library. Machine-specific installation records are
written under ignored `build/`.

Default configuration is `$XDG_CONFIG_HOME/cliproxyapi/config.yaml`, falling back
to `~/.config/cliproxyapi/config.yaml`. Plugin and credential paths are read from
that configuration; dashboard HTML defaults to its sibling `static/` directory.
`--dashboard` overrides that location if the host uses `MANAGEMENT_STATIC_PATH`.

Flags and corresponding environment variables are shared by deployment tools:

| Setting | Flag | Environment variable |
| --- | --- | --- |
| Configuration | `--config` | `CLI_PROXY_CONFIG` |
| CPA executable | `--binary` | `CLI_PROXY_BINARY` |
| Proxy URL | `--url` | `CLI_PROXY_URL` |
| Credential directory | `--auth-dir` | `CLI_PROXY_AUTH_DIR` |
| Plugin directory | `--plugins-dir` | `CLI_PROXY_PLUGIN_DIR` |
| Built native library | `--library` | `CURSOR_LOCAL_LIBRARY` |
| Installed dashboard | `--dashboard` | `CLI_PROXY_DASHBOARD` |
| Built dashboard HTML | `--dashboard-source` | `CURSOR_LOCAL_DASHBOARD_SOURCE` |
| Management key file | `--management-key-file` | `CLI_PROXY_MANAGEMENT_KEY_FILE` |
| Client key file | `--client-key-file` | `CLI_PROXY_CLIENT_KEY_FILE` |

## Sign in, including a headless server

```sh
./build/linux/amd64/cursor-login --no-browser
```

Open the temporary URL printed by the helper on a browser-equipped machine and
approve access. The helper continues polling the server. The saved provider
credentials remain in CPA; management keys are read from files, never argv.
Without `--no-browser`, the helper uses the native desktop browser launcher.
Use `--url` and `--management-key-file` for a different connection. Authenticated
remote helper access requires HTTPS or a loopback SSH tunnel.

Select models from the live proxy catalog, for example `cursor/<model-id>`.
CPA translates Chat Completions, Responses and Anthropic requests into the
provider contract. This serves Cursor subscription models through the proxy.

## Quota and management

The independent management page is at
`/v0/resource/plugins/cursor-local/status`. Enter the CPA management key to view
accounts or change model selections. The static page contains no account data;
only language preference is saved. Lock clears the key and account view.

The customized official dashboard adds Cursor quota cards and a billing-window
timeline. Read-only Cursor dashboard RPCs supply the plan, distinct model usage
allowances, Grok Bot weekly usage, included balance, reset and on-demand status.
Results are cached for one minute per account/token. Missing values stay unknown,
and no future monthly intervals are invented. Local estimates stay separate.
Automatic dashboard replacement is disabled; rebuild the patched dashboard
when incorporating upstream updates.

## Runtime behavior and boundaries

Text, SSE, inline images, file input, image generation and client function tools
are implemented. Tools execute on the client. Checkpoints can reuse matching
text-only history; account/model/history isolation prevents cross-account reuse.
Unsupported native shell/read/grep/web/VM operations are refused. Image asset
writes remain in bounded virtual memory, with no host filesystem writes.

HTTP destinations are fixed to Cursor's API, TLS verification stays enabled,
redirects are refused and remote attachments are not fetched. Credentials and
prompts are not logged by this plugin. There is no telemetry or remote updater.
A native plugin has the CPA process's OS privileges; independent source and
passing checks are not a security guarantee. Cursor's private API may change.

## Ubuntu deployment and updates

See [deploy/UBUNTU.md](deploy/UBUNTU.md) for the Intel laptop, boot persistence,
remote administration and migration. GitHub Actions compile and check Ubuntu
and macOS builds, then load them in a checksum-verified CPA runtime. Action
references are pinned. Browser-dependent login is not part of CI.
