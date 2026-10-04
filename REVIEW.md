# Reference review and implementation provenance

Reviewed 2026-10-03: `yobo2u/omsub`, branch `cursor`, commit
`c43af3c564d444067401e013a74f7a1ec35549a8`. Source reports version 0.6.6;
its README still describes 0.6.5.

The functionality is credible as an experimental Cursor-subscription adapter.
The inspected source had no obvious third-party credential destination,
telemetry, remote code download, or shell subprocess execution. Its race-enabled
mock tests and `go vet` passed. This is a source review, not proof that released
binaries match source or that live subscriptions remain compatible.

## Findings

1. **Should fix for this machine: released installer cannot run on macOS.**
   `cursor-plugin/packaging/install.sh:25` accepts only Linux and packages a `.so`.
   The user's machine is Darwin arm64. A native `.dylib` build and isolated host
   loading test are needed. No upstream installer was executed.
2. **Should fix: OAuth calls have no explicit default timeout.**
   `cursor-plugin/internal/plugin/handler.go:26` uses `http.DefaultClient`, and
   `cursor-plugin/main.go:106` dispatches on `context.Background()`. The OAuth
   HTTP methods inherit that unbounded context. A stalled auth server can hold
   a native host call indefinitely. Use a bounded context/client timeout.
3. **Should fix: shutdown does not cancel active operations.**
   `cursor-plugin/main.go:123` has an empty shutdown callback. Streaming runs
   start goroutines from a background context. The source includes per-run
   deadlines, but shutdown itself has no explicit cancel-and-join mechanism.
   Track active calls and cancel/join them before releasing host callbacks.
4. **Operational limitation: private protocol and estimated accounting.**
   `cursor-plugin/internal/cursorapi/client.go:28` identifies an older CLI
   version. An embedded descriptor, private OAuth paths, and undocumented
   AgentService RPCs require maintenance. Context/token metadata is not an
   authoritative subscription balance. Validate live login, streaming, and tool
   continuation after upstream changes.

Positive evidence: fixed Cursor HTTPS endpoints, TLS validation, bounded
discovery/frame/blob paths, management APIs relying on host authentication,
management keys kept in page memory, and native shell/read/grep execution
refusals. Image output has a separate bounded file-write path under Cursor's
project assets directory. The native library remains fully trusted process code.

## Independent implementation

The user chose independent source rather than a local fork. No Go implementation,
test source, embedded descriptor, HTML, or packaging code from omsub was copied
into this project. Reviewing its source necessarily informed protocol behavior;
this is not a legal clean-room claim.

The new implementation was written directly against these references:

- Installed official Cursor CLI bundle, version `2026.09.02-c22c1a3`: protobuf
  field definitions for AgentClientMessage, AgentRunRequest, AgentServerMessage,
  MCP tools, request context, and key/value messages; CLI PKCE login and refresh.
- [CLIProxyAPI 8.0.10 public plugin ABI](https://github.com/router-for-me/CLIProxyAPI/tree/6fecc6e5567912661654a4eaf9b8f5436facd1c2/examples/plugin/simple):
  required C function table and host RPC JSON fields. The ABI declarations are
  required interoperability declarations, not original omsub implementation.
- Google protobuf `protowire`, `proto`, and `structpb` for standard wire encoding.

The first local build omitted several reference features. That scope reduction
was not the user's request and has been corrected in the independent source.
[PARITY.md](PARITY.md) records the implemented feature set and differences,
including the virtual image workspace and explicit resource limits. The host
request observer is necessary for final logical request accounting, and the
management APIs rely on CPA's management-key authentication.

Links to inspected code:

- [Authentication setup](https://github.com/yobo2u/omsub/blob/c43af3c564d444067401e013a74f7a1ec35549a8/cursor-plugin/internal/plugin/handler.go#L26)
- [Native bridge and shutdown](https://github.com/yobo2u/omsub/blob/c43af3c564d444067401e013a74f7a1ec35549a8/cursor-plugin/main.go#L106)
- [Installer platform restriction](https://github.com/yobo2u/omsub/blob/c43af3c564d444067401e013a74f7a1ec35549a8/cursor-plugin/packaging/install.sh#L25)
