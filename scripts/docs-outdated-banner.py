#!/usr/bin/env python3
"""Adds the "older version" banner to docs versions published before
overrides/main.html existed.

Material for MkDocs puts an empty, hidden <div data-md-component="outdated">
on every page of a versioned site, and its script shows that div on versions
that extra.version.default does not name as current. Versions built since
overrides/main.html fill the div with the banner; versions built before it
have the div but nothing in it. This fills the div in each HTML page of the
versions you name, in a checkout of the gh-pages branch, with the banner
text from overrides/main.html and a link to the page's canonical URL, which
names /latest/. Material's own script, already in those pages, decides
whether to show it, so the banner stays hidden on the version that is
latest and appears once a newer release takes the alias.

Pages that already have the banner are left alone, so running it twice
changes nothing.

Run, then review, commit and push gh-pages:

    git worktree add ../gh-pages gh-pages
    python3 scripts/docs-outdated-banner.py ../gh-pages 1.0 1.1 1.2 1.3
    git -C ../gh-pages commit -am "Add the older-version banner to 1.0 to 1.3"
    git -C ../gh-pages push origin gh-pages
"""

import html
import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
TEMPLATE = ROOT / "overrides" / "main.html"
LATEST = "https://hangar.page/latest/"

# The placeholder Material 9 renders when the outdated block is empty.
EMPTY_DIV = re.compile(r'(<div data-md-color-scheme="default" data-md-component="outdated" hidden>)\s*(</div>)')
FILLED_DIV = re.compile(r'<div data-md-color-scheme="default" data-md-component="outdated" hidden>\s*<aside')
CANONICAL = re.compile(r'<link rel="canonical" href="([^"]+)">')
BLOCK = re.compile(r"{%\s*block outdated\s*%}(.*?){%\s*endblock\s*%}", re.S)
HREF_EXPR = re.compile(r"{{.*?}}", re.S)


def banner_text():
    """The outdated block of overrides/main.html, split around its one
    expression, the link target: (before, after)."""
    match = BLOCK.search(TEMPLATE.read_text(encoding="utf-8"))
    if not match:
        sys.exit(f"error: {TEMPLATE} has no outdated block")
    parts = HREF_EXPR.split(match.group(1).strip())
    if len(parts) != 2:
        sys.exit(f"error: the outdated block in {TEMPLATE} must have exactly one expression, the link target")
    return parts[0], parts[1]


def banner(text, href):
    """The markup Material renders around the outdated block."""
    before, after = text
    return (
        '<aside class="md-banner md-banner--warning">'
        '<div class="md-banner__inner md-grid md-typeset">'
        + before + html.escape(href, quote=True) + after
        + "</div></aside>"
    )


def fill(page, text):
    """Fills the empty banner div in page. Returns True if it changed."""
    source = page.read_text(encoding="utf-8")
    if FILLED_DIV.search(source) or not EMPTY_DIV.search(source):
        return False
    canonical = CANONICAL.search(source)
    href = html.unescape(canonical.group(1)) if canonical else LATEST
    filled = EMPTY_DIV.sub(lambda m: m.group(1) + banner(text, href) + m.group(2), source, count=1)
    page.write_text(filled, encoding="utf-8")
    return True


def main(argv):
    if len(argv) < 3:
        sys.exit("usage: docs-outdated-banner.py GH_PAGES_DIR VERSION...")
    site = pathlib.Path(argv[1])
    text = banner_text()
    for version in argv[2:]:
        folder = site / version
        if not folder.is_dir() or not (folder / "index.html").is_file():
            sys.exit(f"error: {folder} is not a published docs version")
        pages = sorted(folder.rglob("*.html"))
        changed = sum(fill(p, text) for p in pages)
        print(f"{version}: added the banner to {changed} of {len(pages)} pages")


if __name__ == "__main__":
    main(sys.argv)
