"""Resolve dataset layouts with the `datasets` library for compat/viewer.

Usage: python compat/viewer/oracle/layout_cases.py CASES_JSON WORK_DIR

CASES_JSON is a list of {"name": ..., "files": {path: text or null},
"archives": {path: [entry names]}}. For each case the script writes the
files under WORK_DIR/<name> (a null content becomes a few bytes of the
right format; resolution only looks at names and the card) and each zip
archive with its entries in order (a name ending in "/" is a folder
entry), resolves Configs and Splits as
LocalDatasetModuleFactory does, and prints one JSON object mapping each
case to {"configs": [{"name", "default", "builder", "splits", "files"}]}
or {"error": <exception class name>}. Run it with datasets 5.1.0, the
version Hangar's layout package follows:

    uv run --no-project --with datasets==5.1.0 --with pandas==2.3.3 \\
        --with pyarrow==24.0.0 python -I layout_cases.py CASES WORK
"""

from __future__ import annotations

import json
import sys
import zipfile
from pathlib import Path

from datasets import DownloadConfig
from datasets.load import LocalDatasetModuleFactory

TINY = {
    ".csv": "a\n1\n",
    ".tsv": "a\n1\n",
    ".json": '{"a": 1}\n',
    ".jsonl": '{"a": 1}\n',
    ".txt": "a\n",
}


def write(root: Path, files: dict[str, str | None], archives: dict[str, list[str]]) -> None:
    for rel, text in files.items():
        p = root / rel
        p.parent.mkdir(parents=True, exist_ok=True)
        if text is None:
            text = TINY.get(p.suffix, "x")
        p.write_text(text, encoding="utf-8")
    for rel, names in archives.items():
        p = root / rel
        p.parent.mkdir(parents=True, exist_ok=True)
        with zipfile.ZipFile(p, "w") as z:
            for name in names or []:
                info = zipfile.ZipInfo(name, date_time=(1980, 1, 1, 0, 0, 0))
                z.writestr(info, "" if name.endswith("/") else TINY.get(Path(name).suffix, "x"))


def resolve(root: Path) -> dict:
    module = LocalDatasetModuleFactory(root.as_posix()).get_module()
    params = module.builder_configs_parameters
    builder = module.module_path.split("packaged_modules.")[1].split(".")[0]
    configs = []
    default = params.default_config_name
    if default is None and len(params.builder_configs) == 1:
        default = params.builder_configs[0].name
    for cfg in params.builder_configs:
        cfg._resolve_data_files(base_path=root.as_posix(), download_config=DownloadConfig())
        files = {}
        for split, lst in cfg.data_files.items():
            files[str(split)] = [Path(f).resolve().relative_to(root.resolve()).as_posix() for f in lst]
        configs.append(
            {
                "name": cfg.name,
                "default": cfg.name == default,
                "builder": builder,
                "splits": list(files),
                "files": files,
            }
        )
    return {"configs": configs}


def main() -> int:
    cases = json.loads(Path(sys.argv[1]).read_text())
    work = Path(sys.argv[2])
    out = {}
    for case in cases:
        root = work / case["name"]
        write(root, case["files"], case.get("archives") or {})
        try:
            out[case["name"]] = resolve(root)
        except Exception as e:  # noqa: BLE001 - the class is the oracle's answer
            out[case["name"]] = {"error": type(e).__name__, "message": str(e)[:300]}
    json.dump(out, sys.stdout, indent=1)
    return 0


if __name__ == "__main__":
    sys.exit(main())
