"""MkDocs hooks: publish api/openapi.yaml next to the native API page."""

import pathlib

from mkdocs.structure.files import File

ROOT = pathlib.Path(__file__).resolve().parent.parent


def on_files(files, config, **kwargs):
    files.append(File.generated(config, "reference/openapi.yaml", abs_src_path=str(ROOT / "api" / "openapi.yaml")))
    return files
