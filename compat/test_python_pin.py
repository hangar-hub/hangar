"""Checks the default compat environment runs the pinned Python."""

import sys
from pathlib import Path

COMPAT = Path(__file__).resolve().parent


def pinned_version() -> tuple[int, int]:
    major, minor = (COMPAT / ".python-version").read_text().strip().split(".")[:2]
    return int(major), int(minor)


def test_python_pin_matches_lock_cell():
    # compat/hf/README.md documents the lock cell as Python 3.11. Without the
    # pin, uv may build compat/.venv on any Python that meets requires-python,
    # and on macOS the SciPy wheel the lock picks for 3.10 fails to load.
    assert pinned_version() == (3, 11)


def test_default_environment_uses_pinned_python():
    # The matrix cells point UV_PROJECT_ENVIRONMENT elsewhere and choose their
    # own Python; only compat/.venv, the environment `make test-compat` and the
    # lock cell use, must follow the pin.
    in_default_env = Path(sys.prefix).resolve() == (COMPAT / ".venv").resolve()
    assert not in_default_env or sys.version_info[:2] == pinned_version()
