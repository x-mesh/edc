---
description: Release edc (pick version, verify, tag, write release notes)
argument-hint: "[version]"
---

# /release

Call the `edc-release` skill with the Skill tool to release edc. Follow the skill procedure. Do not copy the procedure into this file.

## Arguments

User provided: `$ARGUMENTS`

- If empty, read the changes since the latest tag and propose the next version.
- If a `<version>` is given, release that version.

Ask the user to confirm before a step that is hard to undo, such as a tag push.
