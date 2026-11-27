---
description: Safely release edc through main with repository-wide stable-version checks
argument-hint: "[version]"
---

# /release

Use the edc-release skill. $ARGUMENTS is an optional requested version.

Do not use git describe to select the version. Verify the candidate is greater than every published stable GitHub release before any tag is created.
