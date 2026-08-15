# Customized Release Notes

Every `czc-*` production tag must have a matching Markdown file in this
directory. The filename must be the exact tag followed by `.md`, for example:

```text
.github/releases/czc-v2026.08.15.2.md
```

The first line is the GitHub Release title and must be an H1 heading. Release
notes must be based on the diff from the previous customized production tag and
must include:

- a concise user-visible summary;
- fixes, features, and explicitly unchanged behavior;
- guidance about who should upgrade and who can defer;
- database, configuration, and API compatibility impact;
- tests and production checks that were actually completed;
- rollback availability and a full comparison link.

The `Publish CZC Release Notes` workflow creates or updates the matching GitHub
Release when a note file reaches `main-czc` or when a `czc-*` tag is pushed.
