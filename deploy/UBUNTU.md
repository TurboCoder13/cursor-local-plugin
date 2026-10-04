# Ubuntu Intel laptop deployment

The laptop's native target is `linux/amd64`. Build from source on it, or use a
Linux amd64 artifact from a successfully validated build. A Mac `.dylib`
cannot be reused on Linux.

## Why a laptop at home

The proxy holds the subscription logins for every other machine, so where it
runs matters:

- Traffic to Anthropic and OpenAI leaves from one residential IP address, no
  matter which machine sent the prompt. Sign-ins from datacenter ranges
  (Hetzner, AWS and similar) draw extra scrutiny and can get accounts banned,
  so do not host the proxy on a VPS or behind a VPN.
- Agents keep running when the laptop that started them is closed. Coding
  agents mostly wait on the network, so an old machine is enough: 8 GB of RAM
  and four threads is comfortable, and 4 GB still handles two or three agent
  threads. Parallel compiles and test suites are what saturate it; run those
  in CI instead.
- Linux handles many parallel agents better than macOS, which throttles on
  file-system and security overhead.

The HP EliteBook Folio 1040 G2 used here has a 2-core/4-thread Broadwell CPU
and soldered RAM. Check `lscpu` and `free -h`, inspect the battery for
swelling after years on the charger, and watch temperatures in `btop` during
the first heavy builds.

Use the Claude subscription logins only from Claude Code, and the Codex
logins only from Codex. Routing them into other clients, or serving traffic
from other people, is what gets accounts banned. Never expose the proxy to
users of your own products; that is API work.

## Prepare the laptop

After a fresh Ubuntu install, make the machine stay up and reachable:

```sh
sudo apt-get update && sudo apt-get upgrade -y
sudo apt-get install -y openssh-server ripgrep fd-find jq tmux btop git curl \
  build-essential make ruby ruby-minitest gh unattended-upgrades
sudo systemctl enable --now ssh
```

Ignore the lid and never suspend. `logind` applies without a desktop session,
so this works on Server and Desktop installs:

```sh
sudo mkdir -p /etc/systemd/logind.conf.d
printf '[Login]\nHandleLidSwitch=ignore\nHandleLidSwitchExternalPower=ignore\nHandleLidSwitchDocked=ignore\n' \
  | sudo tee /etc/systemd/logind.conf.d/lid.conf
sudo systemctl mask sleep.target suspend.target hibernate.target \
  hybrid-sleep.target
sudo systemctl restart systemd-logind
```

On Ubuntu Desktop, also set *Automatic Suspend* to off in Power settings. In
the BIOS (F10 at power-on), set the machine to power on after AC loss if the
option exists.

Add swap so several agent threads fit alongside builds on small RAM:

```sh
sudo fallocate -l 8G /swapfile && sudo chmod 600 /swapfile
sudo mkswap /swapfile && sudo swapon /swapfile
echo '/swapfile none swap sw 0 0' | sudo tee -a /etc/fstab
```

Each agent thread gets its own git worktree with its own `node_modules` or
`.venv`, so disk fills faster than CPU. Install Bun and `uv` so dependencies
hard-link from a shared cache, and prune old worktrees periodically.

Install Go from the official distribution at the `go.mod` version or newer
(Ubuntu's packaged Go may be too old), then Bun at the dashboard's
`package.json` version and `uv`:

```sh
curl -fsSL https://bun.sh/install | bash
curl -LsSf https://astral.sh/uv/install.sh | sh
```

## Tailscale

Tailscale makes the proxy reachable from every machine you own, encrypts the
link, and keeps it off the public internet. The tailnet replaces the SSH
tunnel for day-to-day use.

```sh
curl -fsSL https://tailscale.com/install.sh | sh
sudo tailscale up --ssh
tailscale ip -4
tailscale status
```

Approve the device in the admin console if your tailnet requires it, and
enable MagicDNS so clients can use the hostname instead of the IP. Keep the
laptop's SSH reachable over the tailnet only once this works; LAN SSH is a
fallback.

## Prepare and build

Clone the independent provider and the customized management dashboard. The
provider is private, so authenticate GitHub CLI on the new machine first:

```sh
gh auth login
gh repo clone TurboCoder13/cursor-local-plugin
gh repo clone TurboCoder13/Cli-Proxy-API-Management-Center -- --branch cursor-local
```

Keep the dashboard's upstream remote for deliberate update merges. Its `main`
branch remains separate from the `cursor-local` customization branch. In the
dashboard checkout:

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

Skip this section when migrating an existing install; see the migration
section below instead.

## Bind to the tailnet and tune routing

The generated config listens on loopback. Edit
`~/.config/cliproxyapi/config.yaml` so the API is reachable over Tailscale
and so a thread stays on one account:

```yaml
server:
  host: "100.x.y.z" # the laptop's Tailscale IPv4 from `tailscale ip -4`
  port: 8317

routing:
  strategy: "fill-first"
  session-affinity: true
  session-affinity-ttl: "1h"
  session-affinity-subagents: true

management:
  allow-remote: false
```

Binding to the Tailscale address keeps the port off the LAN and the public
internet, and the tailnet encrypts the traffic, so `server.tls` can stay off.
Client keys remain required. With only tailnet peers able to connect, that key
is the second layer, not the only one.

`fill-first` drains one account before touching the next instead of spreading
usage evenly. Combined with session affinity, a long thread keeps hitting the
same account, so its prompt cache is reused rather than rebuilt after every
switch. Watch cache read and write counts in the dashboard; if writes climb,
affinity is not working. Which account gets drained first is still the
proxy's choice; prefer the one whose weekly limit resets soonest, because
unused allowance disappears at the reset.

Management stays loopback-only. Reach it through Tailscale SSH port
forwarding when needed:

```sh
ssh -N -L 18317:127.0.0.1:8317 ubuntu-user@hp-laptop
```

Then open `http://127.0.0.1:18317/management.html`. The management key is in
the server's `~/.config/cliproxyapi/management-key`; transfer it through SSH
or your password manager. Do not put real keys in shell command arguments or
Git.

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
Log viewing is enabled in the generated CPA config. The unit waits for
`network-online.target`; if the Tailscale address is not yet assigned at boot
and the bind fails, the restart loop picks it up a few seconds later.

## Sign in from another machine

Sign in to the built-in providers through the dashboard over the SSH tunnel.
For Cursor, run this on the laptop and open its temporary URL on your other
computer:

```sh
./build/linux/amd64/cursor-login --no-browser
```

## Point clients at the laptop

Every machine on the tailnet uses the same base URL. With MagicDNS,
`http://hp-laptop:8317` works; otherwise use the Tailscale IP. The client key
comes from the server's `~/.config/cliproxyapi/client-key`.

Claude Code reads `~/.claude/settings.json`:

```json
{
  "env": {
    "ANTHROPIC_BASE_URL": "http://hp-laptop:8317",
    "ANTHROPIC_AUTH_TOKEN": "<client key>"
  }
}
```

Codex reads `~/.codex/config.toml`. Keep WebSockets on; it cuts the round-trip
overhead noticeably:

```toml
model_provider = "cliproxyapi"

[model_providers.cliproxyapi]
name = "CLIProxyAPI"
base_url = "http://hp-laptop:8317/v1"
wire_api = "responses"
supports_websockets = true

[model_providers.cliproxyapi.auth]
# client key, as the existing Mac config does it
```

Both clients also work on the laptop itself with the same settings. To keep a
plain, directly-authenticated install alongside, give it a separate home
directory (`CLAUDE_CONFIG_DIR`, `CODEX_HOME`) and a shell alias.

Existing model aliases keep their IDs. Select Cursor models from the live
catalog as `cursor/<model-id>`.

## Run agents on the laptop

The laptop is also the box that agents run on, so threads survive closing the
MacBook. Install the CLIs, then the T3 Code server:

```sh
bun install -g @anthropic-ai/claude-code @openai/codex
bunx t3 serve --tailscale
```

`t3 serve` exposes the machine over the tailnet so the desktop and mobile
apps can start threads on it without T3's relay. See `bunx t3 serve --help`
for the current flags. Run it as a user systemd service in the same way as
the proxy so it comes back after a reboot. Threads start in their own git
worktrees by default; let the agents sort out branch collisions themselves.

Set the data-sharing opt-out in both Claude Code and Codex settings on each
account after sign-in.

## Move existing accounts and GLM settings

Account credentials and the real config travel separately from the repos.
On the Mac they are `~/.cli-proxy-api/` (OAuth JSON files and logs) and
`/opt/homebrew/etc/cliproxyapi.conf`. CPA may refresh OAuth credentials, so
stop the old instance before operating the copied accounts on the new one:

```sh
brew services stop cliproxyapi
rsync -av --exclude logs ~/.cli-proxy-api/ hp-laptop:~/.cli-proxy-api/
scp /opt/homebrew/etc/cliproxyapi.conf hp-laptop:~/.config/cliproxyapi/config.yaml
```

In the copied config, adjust `oauth.auth-dir` and `plugins.dir` to absolute
Ubuntu paths, apply the tailnet and routing changes above, and keep the
existing provider definitions, model aliases and GLM Flash endpoint/key
outside Git.

Do not run fresh setup over the copied config. Install with `--config`,
install the dashboard, and start the new service:

```sh
ruby tools/install.rb --config "$HOME/.config/cliproxyapi/config.yaml" \
  --binary "$PWD/build/runtime/cliproxyapi"
ruby tools/install-dashboard.rb --config "$HOME/.config/cliproxyapi/config.yaml" \
  --dashboard-source ../Cli-Proxy-API-Management-Center/dist/index.html
```

Verify models and quota in the dashboard before moving clients away from the
old Mac. Once the Mac's clients point at the laptop and work, remove the
Homebrew service so two instances never refresh the same tokens.

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
