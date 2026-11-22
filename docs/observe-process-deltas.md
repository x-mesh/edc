# Recent RSS changes in top

Run `edc top --process <pid-or-name>` and press Enter to open the selected sample's detail panel. The panel shows recent observed RSS changes for up to three matching processes:

```text
RSS pid 42: 150.0M · +50.0 MB over 2m0s · 3 observed samples
```

The current RSS is the selected row's process observation. The change compares the earliest and latest distinct process observations available in that row's comparable retained segment, capped at ten minutes. The displayed span is the actual endpoint interval, rounded to seconds (milliseconds for subsecond intervals); it can be shorter than ten minutes. No disk history is queried and no rate is extrapolated.

Only the most recent contiguous observed segment for the same filter and process lifetime is compared: an explicit missing or invalid observation stops backward traversal. Contiguous observations do not imply continuous collection between their timestamps.

The count is the number of distinct process observations, not the number of dashboard ticks. The process sampler can reuse a snapshot while resource ticks continue; repeated `ProcessesAt` timestamps count once. Irregularly spaced observations retain their actual elapsed span and observed count. They do not establish continuous collection between those points.

Comparisons require the same PID and a known, equal process start time. An unknown lifetime, changed filter, missing process, invalid collection, unknown observation time or backwards timestamp ends the comparable segment. A process that restarts starts a new comparison segment even if its PID is reused. Fewer than two distinct observations produce an explicit `change unavailable` explanation rather than a synthetic zero change.

When an older dashboard row is selected, the comparison stops at that row. Later samples are not included. Pausing, process candidate limits, filtering and bounded dashboard retention can leave only a short segment available.

These are raw RSS endpoint changes. Positive changes can reflect workload, caches or GC behavior and are not a memory-leak verdict. Decreases and unchanged RSS are reported too. The existing sustained memory-growth diagnostic still uses its separate fifteen-minute bucket/minimum thresholds and can use process memory/footprint rather than RSS. Recorded `edc history growth --process ...` analysis is unchanged.
