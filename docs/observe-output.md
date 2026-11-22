# One-shot observation output

When writing to a terminal, `proc`, one-shot `svc`, `jobs` and `changes` wrap long displayed fields to the available width. Status and probe identity remain visible, followed by complete summary, error, warning and evidence text. Evidence visibility and collection limits remain unchanged: this does not enable verbose collection or reveal suppressed evidence.

Result details reuse the existing plain detail formatter before wrapping, so error/evidence policies and collection annotations remain consistent. Result status headers retain colors when enabled; the summary footer uses plain count/status pairs to keep each pair together on narrow screens.

The layout uses the actual output terminal width. `NO_COLOR` disables colors without disabling width-aware layout. Wide terminals retain compact rows where the complete data fits. Narrow service and change tables use one wrapped item block per row, keeping the full displayed item identity separate from state, numeric values, timestamps and details.

Existing service/container TUI models, selection, refresh behavior and fixed-height rows are unchanged. `svc` can still open its existing browser in an interactive color terminal; the responsive one-shot formatter applies only when the command takes its text-output path.

Pipes and file output retain the existing text format, including existing preview limits. JSON structure and status-driven exit codes are unchanged. Use a pipe or JSON when you need the original one-line representation for tooling.

```sh
edc proc 1234 -v
NO_COLOR=1 edc jobs
edc changes --since 1h
edc svc web.service
edc proc 1234 --json -
edc changes | cat
```

Redaction occurs before wrapping. This prevents an IP address from being split into several lines before it can be masked. Formatting uses field copies and preserves the typed event/service collections used by the original report.

A `next:` command is emitted without inserted line breaks so a quoted argument containing a path remains copyable. The terminal may wrap that line visually; unlike descriptive fields, its output bytes can exceed the screen width. Existing source preview limits and terminal control-character sanitization remain applicable.
