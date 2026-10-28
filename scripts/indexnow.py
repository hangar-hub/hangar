#!/usr/bin/env python3
"""Tells search engines which pages of https://hangar.page/ changed in a
deploy, with IndexNow (https://www.indexnow.org/documentation). Bing,
Yandex, Seznam and Naver share the submissions, so they crawl a changed page
soon after it is published rather than on their next visit.

It compares two commits of the gh-pages branch: the one before a deploy and
the one after. The sitemaps that robots.txt lists at the later commit name
the pages, and a page has changed when it is new, has gone, has a different
<lastmod>, or, for a page whose sitemap gives no <lastmod> (the home page),
has a different file. The docs Worker deploys gh-pages after each push, so
the script first waits until https://hangar.page/ serves those sitemaps and
files as the later commit has them, then submits the changed pages.

The key is the file at the root of gh-pages named <key>.txt that contains
only <key> (deploy/cloudflare/docs-root/), which proves to search engines
that the submission comes from the site.

Run it in a checkout with gh-pages fetched, after `mike deploy --push` and
scripts/docs-root-files.sh --push:

    python3 scripts/indexnow.py BEFORE [AFTER]

BEFORE is the gh-pages commit before the deploy, or "" for none, in which
case every page is new. AFTER defaults to gh-pages. With --dry-run, it
prints the pages it would submit and neither waits nor submits.
"""

import argparse
import json
import re
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
import xml.etree.ElementTree as ET

ENDPOINT = "https://api.indexnow.org/indexnow"
SITEMAP_NS = "{http://www.sitemaps.org/schemas/sitemap/0.9}"
KEY_FILE = re.compile(r"^([0-9a-f]{32})\.txt$")
# How long to wait for the Worker to serve the new commit, and how often to
# check. Workers Builds usually deploys gh-pages within a few minutes.
TIMEOUT = 900
POLL = 15
# Cloudflare refuses Python's default User-Agent (Python-urllib) with 403.
USER_AGENT = "hangar-indexnow/1 (+https://github.com/hangar-hub/hangar)"


def git(*args, check=True):
    result = subprocess.run(["git", *args], capture_output=True, check=False)
    if check and result.returncode != 0:
        sys.exit(f"error: git {' '.join(args)}: {result.stderr.decode().strip()}")
    return result


class Tree:
    """Files of one gh-pages commit, read with git, following the symlinks
    mike makes for aliases such as latest."""

    def __init__(self, ref):
        self.ref = ref

    def path(self, path):
        """The path with any symlinked directory along it resolved."""
        parts = path.split("/")
        for i in range(1, len(parts)):
            prefix = "/".join(parts[:i])
            entry = git("ls-tree", self.ref, prefix, check=False).stdout.decode().split()
            if entry and entry[0] == "120000":
                target = git("cat-file", "blob", entry[2]).stdout.decode().strip()
                return self.path("/".join([target, *parts[i:]]))
        return path

    def read(self, path):
        """The file's bytes, or None if the commit does not have it."""
        result = git("cat-file", "blob", f"{self.ref}:{self.path(path)}", check=False)
        return result.stdout if result.returncode == 0 else None

    def root_files(self):
        out = git("ls-tree", "--name-only", self.ref).stdout.decode()
        return out.splitlines()


def site_path(url):
    """The path in gh-pages that serves a URL on the site."""
    path = urllib.parse.urlsplit(url).path.lstrip("/")
    if path == "" or path.endswith("/"):
        path += "index.html"
    return path


def sitemaps(tree):
    """The sitemap URLs that robots.txt lists."""
    robots = tree.read("robots.txt")
    if robots is None:
        sys.exit(f"error: {tree.ref} has no robots.txt")
    return re.findall(r"(?im)^Sitemap:\s*(\S+)", robots.decode())


def pages(tree, urls):
    """URL to <lastmod>, or None where the sitemap gives none, for every page
    in the sitemaps."""
    found = {}
    for sitemap in urls:
        raw = tree.read(site_path(sitemap))
        if raw is None:
            continue
        for entry in ET.fromstring(raw).iter(SITEMAP_NS + "url"):
            loc = entry.findtext(SITEMAP_NS + "loc").strip()
            lastmod = entry.findtext(SITEMAP_NS + "lastmod")
            found[loc] = lastmod.strip() if lastmod else None
    return found


def changed_pages(before, after):
    urls = sitemaps(after)
    new = pages(after, urls)
    old = pages(before, urls) if before else {}
    changed = []
    for url, lastmod in new.items():
        if url not in old:
            changed.append(url)
        elif lastmod is None:
            if after.read(site_path(url)) != before.read(site_path(url)):
                changed.append(url)
        elif lastmod != old[url]:
            changed.append(url)
    # Pages that have gone, so search engines see them gone.
    changed += [url for url in old if url not in new]
    return sorted(changed), urls, new


def key(tree):
    for name in tree.root_files():
        match = KEY_FILE.match(name)
        if match and tree.read(name).decode().strip() == match.group(1):
            return match.group(1)
    sys.exit(f"error: {tree.ref} has no IndexNow key file at its root")


def fetch(url):
    """The body the site serves for a URL, or the reason it served none."""
    request = urllib.request.Request(url, headers={"Cache-Control": "no-cache", "User-Agent": USER_AGENT})
    try:
        with urllib.request.urlopen(request, timeout=30) as response:
            return response.read(), None
    except urllib.error.HTTPError as err:
        return None, f"HTTP {err.code}"
    except (urllib.error.URLError, TimeoutError) as err:
        return None, str(err)


def wait_until_served(tree, urls, before=None):
    """Waits until the site serves each URL as the commit has it. A file the
    site serves as the earlier commit has it means the deploy, or a cache in
    front of it, has not caught up; one that matches neither means something
    changes the file on its way."""
    deadline = time.monotonic() + TIMEOUT
    pending = {u: None for u in urls}
    while True:
        for url in list(pending):
            body, failure = fetch(url)
            if body == tree.read(site_path(url)):
                del pending[url]
            elif failure:
                pending[url] = failure
            elif before and body == before.read(site_path(url)):
                pending[url] = f"the file from {before.ref}"
            else:
                pending[url] = f"a file that matches neither commit, {len(body)} bytes"
        if not pending:
            return
        if time.monotonic() > deadline:
            found = "; ".join(f"{u} ({why})" for u, why in pending.items())
            sys.exit(f"error: after {TIMEOUT}s the site still does not serve {tree.ref} for: {found}")
        time.sleep(POLL)


def submit(host, key_name, urls):
    body = json.dumps({
        "host": host,
        "key": key_name,
        "keyLocation": f"https://{host}/{key_name}.txt",
        "urlList": urls,
    }).encode()
    request = urllib.request.Request(
        ENDPOINT, data=body, method="POST",
        headers={"Content-Type": "application/json; charset=utf-8", "User-Agent": USER_AGENT},
    )
    try:
        with urllib.request.urlopen(request, timeout=60) as response:
            status = response.status
    except urllib.error.HTTPError as err:
        sys.exit(f"error: IndexNow answered {err.code}: {err.read().decode(errors='replace').strip()}")
    print(f"IndexNow accepted {len(urls)} pages (HTTP {status}).")


def main():
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    parser.add_argument("before", help='the gh-pages commit before the deploy, or "" for none')
    parser.add_argument("after", nargs="?", default="gh-pages", help="the gh-pages commit after it")
    parser.add_argument("--dry-run", action="store_true", help="print the changed pages only")
    args = parser.parse_args()

    after = Tree(args.after)
    before = Tree(args.before) if args.before else None
    changed, sitemap_urls, new = changed_pages(before, after)
    for url in changed:
        print(url)
    if args.dry_run:
        return
    if not changed:
        print("No pages changed.")
        return
    key_name = key(after)
    # The sitemaps, and the changed pages they give no <lastmod> for, show
    # whether the Worker has deployed this commit.
    wait_until_served(after, sitemap_urls + [u for u in changed if new.get(u, "") is None], before)
    hosts = {urllib.parse.urlsplit(u).hostname for u in changed}
    if len(hosts) != 1:
        sys.exit(f"error: the pages span several hosts: {sorted(hosts)}")
    submit(hosts.pop(), key_name, changed)


if __name__ == "__main__":
    main()
