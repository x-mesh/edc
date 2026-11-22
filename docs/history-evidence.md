# Saved evidence around an observation

Open `edc history` in a terminal, choose a recorded top run, press `t` for its observations, select a sample, and press `e`. The existing overview opens for that run and the selected sample's five-minute window on either side. The range is intersected with the run's recorded samples. Esc returns to the selected observation.

For an explicit range or non-interactive output:

```sh
edc history overview --run RUN_ID --from 2026-11-23T01:25:00Z --to 2026-11-23T01:35:00Z
edc history overview --run RUN_ID --from 2026-11-23T01:25:00Z --to 2026-11-23T01:35:00Z --json -
```

The saved-evidence section combines:

- Structured event timestamps from saved `events.window` snapshot results.
- Timestamped package, configuration and service changes from saved `changes.*` results.
- Recorded command-log metadata whose start is inside the range, or whose known execution duration overlaps it.

Entries are ordered by actual event time. Snapshot capture time is retained separately as `captured_at` in JSON, so a later snapshot can contribute an earlier event. A selected run only receives snapshot evidence with exactly the same stored hostname. A multi-host overview labels each snapshot entry with its recorded hostname.

Command-log records do not store host identity. Each such timeline entry has `host_unknown: true` and is labeled as unscoped archive evidence, not as evidence from the selected run's host. An overlap-only command keeps its original start and end times and has `overlap: true`; an unknown end is never assumed to overlap. The existing command-log list (`l`, then Enter) keeps its start-time matching behavior. An overlap-only entry can be inspected through its saved `log_key`/`log_path` in JSON and the command-log archive.

These records overlap in time; they do not establish causality. The history reader does not run commands or recollect events or changes from the current host.

New Linux and macOS event snapshots store timezone-bearing structured event times. Older event snapshots contain only localized, timezone-less Evidence text. They are reported as unavailable for time correlation rather than parsed using the current timezone. The existing Evidence remains available through snapshot export and report viewing.

The reader reports saved SKIP results, malformed data, missing snapshot tables, legacy timestamps, source warnings and truncation. An empty section means no usable saved evidence was found; it does not mean that no incident occurred.

Read limits are 1,000 candidate saved results, 8 MiB per result, 32 MiB total saved data and 200 timeline entries. Snapshot evidence is retained before unscoped command logs when the entry budget is exhausted; the final display remains chronological. Repeated snapshot evidence is deduplicated. Any applicable bound is disclosed in the evidence notices. The existing saved event timeline also discloses its own 50-entry truncation.
