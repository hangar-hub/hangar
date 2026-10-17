#!/usr/bin/env python3
"""Checks that the Hub API compatibility matrix is current.

compat/hf/compatibility-matrix.json is written from a run of the compat
suites, so it goes stale when a test is added, renamed or removed and nobody
runs them again with HANGAR_HF_WRITE_MATRIX=1. This compares it with the
tests themselves, without running them:

- every `@pytest.mark.endpoint(...)` on a test in compat/hf/test_*.py, and
  every `recordMatrix(t, ...)` call in a Go test in compat/hf, has its test
  listed under each endpoint it names;
- every test the matrix lists still exists and still names that endpoint.

Test modules that skip themselves at import in the lock environment (for
example test_diffusers.py) run only in other client matrix cells, so their
rows are not required. docs/reference/hub-api-compatibility.md is checked
by `make check-generate`, which renders it from the matrix.

Run: python3 scripts/check-hub-matrix.py
"""

import ast
import json
import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
HF = ROOT / "compat/hf"
MATRIX = HF / "compatibility-matrix.json"
REGENERATE = (
    "Regenerate it: HANGAR_HF_WRITE_MATRIX=1 go test -count=1 -tags compat ./compat/hf, then "
    "HANGAR_HF_INSTANCE=binary HANGAR_HF_WRITE_MATRIX=1 uv run --project compat pytest compat/hf, then make generate."
)


def is_pytest_call(node, names):
    return (
        isinstance(node, ast.Call)
        and isinstance(node.func, ast.Attribute)
        and node.func.attr in names
        and isinstance(node.func.value, ast.Name)
        and node.func.value.id == "pytest"
    )


def skips_at_import(tree):
    """Whether a module calls pytest.skip or pytest.importorskip at top level."""
    for stmt in tree.body:
        if isinstance(stmt, ast.FunctionDef | ast.AsyncFunctionDef | ast.ClassDef):
            continue
        for node in ast.walk(stmt):
            if is_pytest_call(node, {"skip", "importorskip"}):
                return True
    return False


def endpoint_marks(fn):
    for dec in fn.decorator_list:
        # pytest.mark.endpoint("...", ...)
        if (
            isinstance(dec, ast.Call)
            and isinstance(dec.func, ast.Attribute)
            and dec.func.attr == "endpoint"
            and isinstance(dec.func.value, ast.Attribute)
            and dec.func.value.attr == "mark"
        ):
            for arg in dec.args:
                if not (isinstance(arg, ast.Constant) and isinstance(arg.value, str)):
                    raise SystemExit(f"error: {fn.name}: endpoint markers must be string literals")
                yield arg.value


def python_marks():
    """Returns {(endpoint, test)} required, and {(endpoint, test)} allowed."""
    required, allowed = set(), set()
    for path in sorted(HF.glob("test_*.py")):
        tree = ast.parse(path.read_text(), str(path))
        optional = skips_at_import(tree)
        for fn in ast.walk(tree):
            if isinstance(fn, ast.FunctionDef | ast.AsyncFunctionDef) and fn.name.startswith("test_"):
                for ep in endpoint_marks(fn):
                    allowed.add((ep, fn.name))
                    if not optional:
                        required.add((ep, fn.name))
    return required, allowed


GO_FUNC = re.compile(r"^func (Test\w+)\(t \*testing\.T\)", re.M)
GO_RECORD = re.compile(r"\brecordMatrix\(t((?:,\s*\"[^\"]*\")+)\)")


def go_marks():
    marks = set()
    for path in sorted(HF.glob("*_test.go")):
        src = path.read_text()
        funcs = [(m.start(), m.group(1)) for m in GO_FUNC.finditer(src)]
        for m in GO_RECORD.finditer(src):
            owner = [name for start, name in funcs if start < m.start()]
            if not owner:
                raise SystemExit(f"error: {path.name}: recordMatrix outside a Test function")
            for ep in re.findall(r"\"([^\"]*)\"", m.group(1)):
                marks.add((ep, owner[-1]))
    return marks


def main():
    required, allowed = python_marks()
    gm = go_marks()
    required |= gm
    allowed |= gm
    matrix = json.loads(MATRIX.read_text())["endpoints"]
    listed = {(ep, re.sub(r"\[.*\]$", "", t)) for ep, row in matrix.items() for t in row.get("tests", [])}

    errors = []
    for ep, test in sorted(required - listed):
        errors.append(f"{test} covers {ep!r}, but the matrix does not list it there")
    for ep, test in sorted(listed - allowed):
        errors.append(f"the matrix lists {test} under {ep!r}, but no test by that name covers it")
    if errors:
        for e in errors:
            print(f"error: {MATRIX.relative_to(ROOT)}: {e}", file=sys.stderr)
        print(REGENERATE, file=sys.stderr)
        return 1
    print(f"The Hub API compatibility matrix lists all {len(required)} endpoint and test pairs.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
