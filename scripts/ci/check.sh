#!/usr/bin/env bash
# Validate and load the plugin with the checksum-verified, supported CPA release.
set -euo pipefail

if [[ "$(uname -s)" == Linux ]]; then
	sudo apt-get update
	sudo apt-get install -y --no-install-recommends ruby ruby-minitest
fi

make build
make check
ruby tools/fetch-runtime.rb
CLI_PROXY_BINARY="$PWD/build/runtime/cliproxyapi" ruby tools/host-check.rb
