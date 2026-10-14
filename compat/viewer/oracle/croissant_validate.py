"""Validate Croissant files with mlcroissant for compat/viewer.

Usage: python compat/viewer/oracle/croissant_validate.py FILE...

Loads each JSON-LD file with mlcroissant.Dataset, which validates it
against the Croissant 1.0 specification, and prints one JSON object
mapping each file to {"errors": [...], "warnings": [...]}. Loading does
not download the dataset's files. Run it with mlcroissant 1.1.1:

    uv run --no-project --with mlcroissant==1.1.1 python -I croissant_validate.py FILE...
"""

from __future__ import annotations

import json
import sys

import mlcroissant as mlc


def main() -> int:
    out = {}
    for path in sys.argv[1:]:
        result = {"errors": [], "warnings": []}
        try:
            ds = mlc.Dataset(jsonld=path)
            result["warnings"] = sorted(str(w) for w in ds.metadata.ctx.issues.warnings)
            result["errors"] = sorted(str(e) for e in ds.metadata.ctx.issues.errors)
        except mlc.ValidationError as e:
            result["errors"] = [str(e)]
        out[path] = result
    json.dump(out, sys.stdout, indent=1)
    return 0


if __name__ == "__main__":
    sys.exit(main())
