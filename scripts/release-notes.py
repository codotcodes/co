#!/usr/bin/env python3
"""Extract curated release notes from the versioned changelog."""

from pathlib import Path
import re
import sys


def release_notes(changelog, tag):
    if not re.fullmatch(r"v[0-9]+\.[0-9]+\.[0-9]+", tag):
        raise ValueError("expected a stable vMAJOR.MINOR.PATCH tag")
    heading = re.compile(
        r"^## \[" + re.escape(tag[1:]) + r"\] - [0-9]{4}-[0-9]{2}-[0-9]{2}$"
    )
    lines = changelog.splitlines()
    matches = [index for index, line in enumerate(lines) if heading.fullmatch(line)]
    if len(matches) != 1:
        raise ValueError("expected exactly one dated changelog entry for the tag")
    start = matches[0] + 1
    end = next(
        (index for index in range(start, len(lines)) if lines[index].startswith("## ")),
        len(lines),
    )
    notes = "\n".join(lines[start:end]).strip()
    if not notes:
        raise ValueError("release notes must not be empty")
    return notes + "\n"


if __name__ == "__main__":
    if len(sys.argv) != 2:
        raise SystemExit("usage: release-notes.py vMAJOR.MINOR.PATCH")
    try:
        print(release_notes(Path("CHANGELOG.md").read_text(), sys.argv[1]), end="")
    except ValueError as error:
        raise SystemExit(str(error)) from error
