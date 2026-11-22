# Automatic recording completion

Run `edc top --auto-record` to keep the existing five-minute pre-trigger buffer and record when load, swap-out or macOS memory pressure meets the existing trigger rules. Automatic recording closes after one calm minute. Pressing `w` can stop it sooner; it does not restart until the trigger condition clears.

When an automatic run closes, the dashboard shows a short notice with its run ID and committed sample count. After leaving the dashboard with `q` or Ctrl-C, the restored terminal shows the latest automatic recording:

- Observed time window, including its saved pre-trigger backlog and calm tail.
- Original trigger and number of committed samples.
- Host CPU peak and memory-used percentage at the first, peak and last observations.
- The largest readable process RSS observation, with name and PID. This is an observed peak, not a growth rate or a memory-leak diagnosis.
- An `edc history overview --run <id> <DB>` command using the database that received the recording.

If several automatic runs close in one dashboard session, the report names the total and shows only the latest. Earlier runs remain in the history database. Keeping one latest summary and fixed-size per-run statistics avoids retaining every sample or completion report in memory.

The summary counts only samples whose database transactions committed. A failed queue, write or close is labeled incomplete and keeps the existing nonzero exit behavior; available committed observations can still be reported. Missing process readings are shown as unavailable rather than a zero RSS process. A recording with no committed samples says so explicitly.

Manual-only recordings retain their existing output. Stopping a manual recording after an automatic run does not replace the latest automatic report.

The report wraps descriptive fields to the terminal width and works without colors. The review command is emitted without inserted line breaks so a quoted path containing spaces or apostrophes remains copyable; the terminal may wrap it visually. Paths containing control characters are rendered with Bash/Zsh ANSI-C shell quoting instead of sending raw control sequences to the terminal.
