#!/usr/bin/env python3
"""Checks that the tests the docs cite exist where the docs say they are.

Pages such as docs/security/threat-model.md back each claim with the names
of the tests that keep it, written in inline code, such as `TestSealOpen`.
A test file in parentheses after one or more names, such as
`TestA`, `TestB` (`internal/x/x_test.go`), says where those names are. The
names it covers are the ones since the previous parenthesised group, table
cell or semicolon. With several files in the group, each name must be in one
of them. A name with no file after it must be defined in some Go test file in
the repository. A name counts as defined when a `func Name(` line declares it.

Run: python3 scripts/check-test-citations.py [Markdown files...]
With no arguments it checks every Markdown file under docs/ except docs/spec,
docs/research and docs/build, and the Markdown files at the repository root.
"""

import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent

FENCE = re.compile(r"^\s*(```|~~~)")
# A test or fuzz name in a code span, a parenthesised group, a cell border or
# a semicolon, in the order they appear on the line.
TOKEN = re.compile(r"`((?:Test|Fuzz)[A-Z0-9_]\w*)`|\(([^()]*)\)|\||;")
TEST_FILE = re.compile(r"`([\w./-]+_test\.go)`")
SKIP_DIRS = ("docs/spec/", "docs/research/", "docs/build/")


def markdown_files():
    files = [
        p for p in sorted((ROOT / "docs").rglob("*.md"))
        if not p.relative_to(ROOT).as_posix().startswith(SKIP_DIRS)
    ]
    files += sorted(ROOT.glob("*.md"))
    return files


def citations(text):
    """Yields (line number, test name, files) for each cited test.

    files is the tuple of test files the page names for it, or empty when
    the page names none.
    """
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
        pending = []
        for token in TOKEN.finditer(line):
            if token.group(1):
                pending.append(token.group(1))
                continue
            files = ()
            if token.group(2) is not None:
                files = tuple(TEST_FILE.findall(token.group(2)))
            for name in pending:
                yield number, name, files
            pending = []
        for name in pending:
            yield number, name, ()


def declared(path, cache):
    """Returns the test and fuzz names declared in a Go file."""
    if path not in cache:
        names = set()
        if path.is_file():
            for m in re.finditer(r"^func ((?:Test|Fuzz)\w+)\(", path.read_text(encoding="utf-8"), re.M):
                names.add(m.group(1))
        cache[path] = names
    return cache[path]


def all_tests(cache):
    names = set()
    for path in ROOT.rglob("*_test.go"):
        if "node_modules" in path.parts or ".git" in path.parts:
            continue
        names |= declared(path, cache)
    return names


def display(path):
    try:
        return path.relative_to(ROOT)
    except ValueError:
        return path


def main(argv):
    paths = [pathlib.Path(p).resolve() for p in argv] or markdown_files()
    cache = {}
    everywhere = None
    failures = 0
    for path in paths:
        for number, name, files in citations(path.read_text(encoding="utf-8")):
            if files:
                if any(name in declared(ROOT / f, cache) for f in files):
                    continue
                print(f"{display(path)}:{number}: {name} is not in {', '.join(files)}")
            else:
                if everywhere is None:
                    everywhere = all_tests(cache)
                if name in everywhere:
                    continue
                print(f"{display(path)}:{number}: {name} is not defined in any test file")
            failures += 1
    if failures:
        print("Cite each test by its name and the file that defines it.")
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
