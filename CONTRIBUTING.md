# Maintaining this personal plugin

Run `make build`, `make check`, and `make host-check` after source changes.
Run the full `uv run lintro chk`; do not filter it to individual tools.

The only lint exclusions are generated `build/` artifacts and `.lintro/` reports.
Reports contain generated tables and snippets and must not be scanned again as
project source. Native library headers are generated from the C ABI declaration.
No implementation or test file is excluded.

Live acceptance requires a Cursor account. Use a short text request, streaming,
and one harmless client tool followed by its result. Do not treat mock tests or
native loading alone as proof of current subscription compatibility. Avoid
printing credentials, raw auth responses, or upstream error bodies.

Keep OAuth fields and protocol numbers traceable to the installed official
Cursor CLI. Add explicit handling or refusal for new native requests rather than
granting execution on the proxy host.
