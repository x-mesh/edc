# Collection coverage in observe commands

`proc`, `svc`, `jobs`, and `changes` distinguish the health of the observed subject from whether its data could be collected. A service or scheduled job can fail even though its state was successfully observed. A healthy-looking partial snapshot does not establish that unreadable sources are healthy.

Text output keeps the existing pass/warn/fail/skip verdicts and adds a separate collection summary:

```text
Collection: 3 observed · 1 partial · 1 permission denied
3 pass  ·  1 warn  ·  0 fail  ·  1 skip
```

Each collection-summary count is a number of annotated sources/results, not a number of jobs, processes, files, or sockets. Source-specific partial and unreadable details appear next to their results; verbose output also shows successful collection details. The service browser puts collection scope on its own line above the table.

| Collection state | Meaning |
| --- | --- |
| `observed` | The collector successfully observed its documented source scope. This does not assert host-wide completeness or that every optional metric exists. |
| `partial` | Known omissions or a collection limit leave some data unread. Any observed count is a lower bound. |
| `permission_denied` | Typed permission evidence prevents reading the source. No item count is fabricated. |
| `unsupported` | The source/backend is known to be unsupported or unavailable on this platform. |
| `unavailable` | The source could not be read, is unconfigured, or a known subject was not observed. No zero count is invented. |

A successful empty enumeration carries `observed: 0`. An unreadable source omits the observed-item count. `partial` with zero observed items means that nothing was observed from the readable portion; it does not mean that no matching items exist.

Counts are source-specific. For example, `proc.fd` counts descriptors, `svc.fd` counts process FD readings, service lists count returned services before display-only TUI filtering, jobs count scheduled-job records, and changes count events observed in the requested time window. Counts from different sources must not be added together as a host total.

Existing source-scope limitations still apply:

- Service FD inspection is bounded to the selected process readings. A known main PID that is missing from the process sample is not treated as an observed empty service.
- Socket ownership and journals can be restricted by account permissions. A hidden journal with some visible records is partial.
- Service list filters select which services the collector requests; a TUI text or failed-only filter narrows presentation without changing source coverage.
- Cron does not retain the last execution result. That informational limitation alone is not classified as a failed or partial enumeration.
- Launchd collection describes the queried domain and configured log tail, not every domain or the entire log file.
- Changes reports mark known unreadable entries, skipped unsupported rotated formats, and scan-budget truncation as partial when some source data was read.

## JSON compatibility

Existing health verdicts, metrics, errors, warnings and exit-code rules retain their meanings. Affected results gain optional additive metadata:

```json
{
  "collection": {
    "state": "partial",
    "observed": 2
  }
}
```

`reason` is optional and uses the report's existing redaction path. Legacy results and unrelated commands omit `collection`; they are not reclassified from translated summaries, arbitrary warning text, or health verdicts. Saved older reports therefore do not gain guessed collection coverage when displayed.
