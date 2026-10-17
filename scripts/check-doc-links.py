#!/usr/bin/env python3
"""Checks that relative links in the Markdown we write point at real files.

MkDocs checks links only in the pages it builds, and it leaves out
docs/spec, docs/research and docs/build (exclude_docs in mkdocs.yml), so a
broken link there went unnoticed. This checks every Markdown file under
docs/ and the Markdown files at the repository root, including the ones
MkDocs builds, so one check covers them all.

A link counts when it is an inline link or image, `[text](target)`, whose
target has no URL scheme and is not only an anchor. The anchor and any query
are dropped and the rest must name a file or folder, relative to the linking
file. Fenced code blocks and inline code spans are ignored. Files that the
docs build generates (GENERATED) count as present.

Run: python3 scripts/check-doc-links.py [Markdown files...]
"""

import pathlib
import re
import sys
import urllib.parse

ROOT = pathlib.Path(__file__).resolve().parent.parent

# Written into the site by docs/hooks.py, so they are not in the repository.
GENERATED = {ROOT / "docs/reference/openapi.yaml"}

LINK = re.compile(r"!?\[(?:[^\[\]]|\[[^\[\]]*\])*\]\(\s*<?([^)\s>]+)>?(?:\s+\"[^\"]*\")?\s*\)")
FENCE = re.compile(r"^\s*(```|~~~)")
CODE_SPAN = re.compile(r"(`+)(?:(?!\1).)+\1")
SCHEME = re.compile(r"^[a-zA-Z][a-zA-Z0-9+.-]*:")


def markdown_files():
    files = sorted((ROOT / "docs").rglob("*.md"))
    files += sorted(ROOT.glob("*.md"))
    return files


def links(text):
    """Yields (line number, target) for each inline link outside code."""
    fence = None
    for number, line in enumerate(text.splitlines(), start=1):
        match = FENCE.match(line)
        if match:
            if fence is None:
                fence = match.group(1)
            elif match.group(1) == fence:
                fence = None
            continue
        if fence is not None:
            continue
        for link in LINK.finditer(CODE_SPAN.sub("", line)):
            yield number, link.group(1)


def broken(path):
    """Returns (line number, target) for each relative link that resolves to nothing."""
    found = []
    for number, target in links(path.read_text(encoding="utf-8")):
        if SCHEME.match(target) or target.startswith(("#", "/")):
            continue
        relative = urllib.parse.unquote(target.split("#", 1)[0].split("?", 1)[0])
        if not relative:
            continue
        resolved = (path.parent / relative).resolve()
        if resolved.exists() or resolved in GENERATED:
            continue
        found.append((number, target))
    return found


def display(path):
    """Names a file relative to the repository when it is in it."""
    try:
        return path.relative_to(ROOT)
    except ValueError:
        return path


def main(argv):
    paths = [pathlib.Path(p).resolve() for p in argv] or markdown_files()
    failures = 0
    for path in paths:
        for number, target in broken(path):
            print(f"{display(path)}:{number}: broken link {target}")
            failures += 1
    if failures:
        print("Fix the links above so they name files in the repository.")
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
