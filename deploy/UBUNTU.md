# Ubuntu Intel laptop deployment

The laptop's native target is `linux/amd64`. Build from source on it, or use a
Linux amd64 artifact from a successfully validated build. A Mac `.dylib`
cannot be reused on Linux.

## Prepare and build

Install Go matching `go.mod` or newer from the official Go distribution, plus:

```sh
sudo apt-get update
sudo apt-get install -y build-essential make ruby ruby-minitest git gh
```

Clone the independent provider and the customized management dashboard:

```sh
gh repo clone TurboCoder13/cursor-local-plugin
gh repo clone TurboCoder13/Cli-Proxy-API-Management-Center -- --branch cursor-local
```

Keep the dashboard's upstream remote for deliberate update merges. Its `main`
branch remains separate from the `cursor-local` customization branch. The
provider is private, so authenticate GitHub CLI on the new machine first.
The dashboard build requires Bun at its `package.json` version. In the dashboard
checkout:

```sh
bun install --frozen-lockfile
bun run verify
```

In the provider checkout:

```sh
make build
make check
ruby tools/fetch-runtime.rb
make host-check ARGS="--binary $PWD/build/runtime/cliproxyapi"
```

The runtime fetcher uses `deploy/runtime.json` and verifies the official release
checksum. It selects the plugin-capable release, never a `no-plugin` asset.
The helper requires GitHub CLI; manual release download with checksum
verification is also valid.

## Create a fresh deployment

```sh
ruby tools/setup.rb
ruby tools/install.rb
ruby tools/install-dashboard.rb --dashboard-source ../Cli-Proxy-API-Management-Center/dist/index.html
mkdir -p "$HOME/.local/bin" "$HOME/.local/state/cliproxyapi"
install -m 700 build/runtime/cliproxyapi "$HOME/.local/bin/cliproxyapi"
```

The dashboard checkout's name is arbitrary; adjust `--dashboard-source` to
match it. Setup generates private key files under `~/.config/cliproxyapi/`.
It does not overwrite an existing install. Choose explicit paths using the
options documented in the provider README if using XDG overrides.

## Keep it running after logout and at boot

The included unit is a systemd user service. It uses the default paths above;
edit its `ExecStart` and `WorkingDirectory` if choosing other locations.

```sh
mkdir -p "$HOME/.config/systemd/user"
install -m 644 deploy/cliproxyapi.service "$HOME/.config/systemd/user/cliproxyapi.service"
systemctl --user daemon-reload
systemctl --user enable --now cliproxyapi
sudo loginctl enable-linger "$USER"
systemctl --user status cliproxyapi
```

The service restarts on failure and writes logs in its working directory.
Log viewing is enabled in the generated CPA config. For a laptop, also disable
lid-close suspend and automatic sleep in Ubuntu's power settings; a running
service cannot keep a suspended computer available.

## Administer and sign in from another machine

By default the generated listener and management API remain on loopback. From
a browser-equipped computer, forward a local port to the Ubuntu server:

```sh
ssh -N -L 18317:127.0.0.1:8317 ubuntu-user@hp-laptop
```

Open `http://127.0.0.1:18317/management.html`. The management key is in the
server's `~/.config/cliproxyapi/management-key`; transfer it through SSH or your
password manager. Do not put real keys in shell command arguments or Git.
Sign in to the built-in providers through the dashboard. For Cursor, run this
on the laptop and open its temporary URL on your other computer:

```sh
./build/linux/amd64/cursor-login --no-browser
```

Remote clients can use the SSH tunnel's OpenAI base URL
`http://127.0.0.1:18317/v1` or Anthropic base URL `http://127.0.0.1:18317` with
the server's client key. Existing aliases can keep their model IDs.
For direct LAN access instead, bind a chosen LAN interface and configure HTTPS
using CPA's `server.tls` settings and a trusted certificate. Client keys must
remain enabled. Management can stay restricted to the SSH tunnel.

## Move existing accounts and GLM settings

Account credentials and the real config travel separately from the repos.
Transfer over SSH, or sign in again on the server. For a copied config, adjust
`oauth.auth-dir` and `plugins.dir` to absolute Ubuntu paths. Keep the existing
provider definitions, model aliases and GLM Flash endpoint/key outside Git.
CPA may refresh OAuth credentials, so stop the old instance before operating
the copied accounts on the new one.

Do not run fresh setup over the copied config. Install with `--config`, install
the dashboard, and start the new service. Verify models and quota before moving
clients away from the old Mac. There is no server SSH address configured in
this repository; deployment to the actual laptop remains a separate step.

## Updates and rollback

Add the upstream remote once in the dashboard checkout, then merge updates
into the customization branch and resolve any conflicts:

```sh
git remote add upstream https://github.com/router-for-me/Cli-Proxy-API-Management-Center.git
git fetch upstream
git switch cursor-local
git merge upstream/main
bun run verify
```

Rebuild the dashboard with Bun and use
`install-dashboard.rb` with the new HTML. Native plugin updates require
`make build check`, `host-check`, installation and service restart. Changing the
pinned CPA release requires the same native loading check before rollout.
Installers retain config and artifact backups next to their destinations.
Source, credentials and server state have separate lifecycles.
