# Feature parity record

Reference: omsub Cursor plugin 0.6.6, pinned to
`c43af3c564d444067401e013a74f7a1ec35549a8`.
The implementation is independent; no upstream implementation, tests, descriptor,
UI, or installer was incorporated. Reading the reference informed the behavior.

This matrix records implemented features and verification. Live text, SSE and
client tool handoff/result continuation passed with `cursor/gpt-5.4-mini-none`.
Other protocol paths have mock coverage. That evidence does not establish every
advertised model or future compatibility; see [deployment validation](deploy/VALIDATION.md).

## Implemented features

### Browser PKCE login and refresh

Bounded OAuth calls; stable persisted identity; retained model and tool guard
settings. Verification: OAuth and refresh tests; real CPA OAuth registration.

### Account-specific usable model discovery

Dynamic `cursor/` routes; disabled models excluded. Verification: Discovery
and disabled execution tests.

### Optional model capacity metadata

Native non-MAX capacity separately published from the 1,000,000-unit client
policy; optional failure retains usable models. Verification: Capacity
availability/failure tests.

### Chat JSON and SSE

Text, tools, images, estimated usage and final markers; empty/error responses
handled explicitly. Verification: HTTP/2 completion and stream tests.

### Function tools and selection

Auto, none, required and forced function catalog; sibling calls returned
together. Verification: Tool choice and multiple-call tests.

### Tool IDs and results

Valid IDs preserved; invalid IDs deterministically bounded; deduplicated
exposure; full result history. Verification: Stable ID and continuation tests.

### Inline images and files

Image URLs, input_image, file/input_file, image and UTF-8 files, tool-result
content parts. Verification: Content decoding and selected-context tests.

### Image generation

Correlated description approval; image-only responses; inline
message.images/delta.images. Verification: Image protocol and completion
tests.

### Image asset writes

Bounded per-run virtual image workspace; path restrictions and collision
isolation; no host file writes. Verification: Virtual path/collision tests.

### MCP catalog re-query

Correlated MCP state and request-context replies; client tools never executed
by the gateway. Verification: Real mock HTTP/2 catalog exchange.

### Native shell/read/grep refusals

Correlated refusals; shell stream start, stderr, exit, failure and close.
Verification: Native refusal sequence tests.

### Interaction policy

Web, Exa, fetch, mode and question refusals; plan error; unsupported
VM/unknown operations fail. Verification: Interaction refusal tests.

### Checkpoint reuse

Account/model/session isolation; canonical prefix match; suffix-only replay;
edited/expired histories invalidate. Verification: HTTP/2 reuse and isolation
tests.

### Safe replay fallback

One retry for checkpoint/state failure before output, tools or interaction
replies; tool-result suffix prevents replay. Verification:
Safe/text/interaction fallback tests.

### Checkpoint limits

15-minute TTL; 64 entries; 16 MiB; text-only completion commits; trailing
checkpoint collection. Verification: Cache and trailing-checkpoint tests.

### Optional tool loop guard

Off by default; three adjacent identical linked exchanges; JSON normalization
and distinct IDs. Verification: Guard and user-message break tests.

### Context and output admission

Full history validated before cache lookup; conservative byte accounting;
whole tool calls and UTF-8 truncation. Verification: Counting, admission and
output limit tests.

### Session queue

Eight followers per session; 64 total session turns; cancellable 30-second
wait; request-scoped 409. Verification: Local/global queue and cancellation
tests.

### Blob budget

4,096 entries, 16 MiB each, 64 MiB total including keys; rejected overwrites
are atomic. Verification: Frame/blob and overwrite tests.

### Watchdogs and shutdown

15-minute run; 30-second frame silence; 90-second meaningful progress; active
run cancellation/join. Verification: Cancellation tests and host loading.

### Management authentication

CPA management-key middleware; public resource contains only static UI.
Verification: Actual CPA authorized/unauthorized API checks.

### Bilingual management page

English/Chinese; language preference only persisted; key kept in memory; lock
clears account view. Verification: T3 browser login, language and lock checks.

### Account filtering and health

Persisted physical records only; physical and connected account/email
deduplication; host status; native subscription quota. Verification: Storage
filtering, identity grouping and status tests.

### Model controls

Save, enable/disable all, confirmation, credential-field preservation;
enforced discovery and execution. Verification: Storage tests and browser
fixture controls.

### Native subscription quota

Read-only dashboard RPCs supply the plan, distinct model usage buckets, Grok Bot
weekly usage, included balance, billing reset and on-demand status. Responses are
bounded and sanitized, cached for one minute per account/token, and concurrent
requests coalesce. Unknown fields remain unknown. The ambiguous hard-limit money
field is omitted. Verification: race-enabled normalization, failure, cache and
account/token isolation tests; authenticated installed CPA quota check.

### Local usage and checkpoints

Estimated execution tokens; per-account cache counters; host attempts shown
separately. Verification: Usage/counter and status tests.

### Terminal logical accounting

Host request.complete; retry deduplication; rotating 16 MiB Bloom windows, at
least 15-minute retention. Verification: Lifecycle and window rotation tests.

## Deliberate implementation differences

Feature parity does not mean identical packaging, code structure, defaults or UI.

- The source and deployment tooling are portable. Native libraries are built
  separately for each server OS/architecture, with no remote updater.
- The management layout and provider identity are our own (`cursor-local`).
- Generated image writes use virtual memory instead of temporary host files.
  Images are returned inline; native filesystem access remains refused.
- In addition to requested output limits, text/thinking/tool output has a 16 MiB
  safety ceiling. This ceiling reports `length`. It does not remove a modality
  or disable tool selection. The reference has no default output byte ceiling.
- The native bridge allows 128 total operations; session-backed turns retain the
  reference's 64-turn global and eight-follower per-session limits.
- The current installed official CLI supplies wire field checks and client version
  headers. The original pins an older descriptor/client version.
- Explicit OAuth deadlines, redirect rejection and cancel/join shutdown address
  the reference review's timeout and lifecycle findings.

These are code-level boundaries within a trusted native CPA process, not an OS
sandbox or a claim that independently written code is automatically secure.
