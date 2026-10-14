"""Fixtures for the Xet upload compatibility suite (compat/xet). See
xetcompat.py for how the suite runs."""

from __future__ import annotations

import shutil
import tempfile
from pathlib import Path

import pytest
from xetcompat import Builder, Client, Server, hf_xet_version


@pytest.fixture(scope="session")
def builder():
    workdir = Path(tempfile.mkdtemp(prefix="hangar-xet-compat-"))
    b = Builder(workdir)
    yield b
    for srv in b.servers:
        b.stop(srv)
    shutil.rmtree(workdir, ignore_errors=True)


@pytest.fixture(scope="session")
def server(builder) -> Server:
    """The default server: ingest mode, default token lifetime, no proxy."""
    return builder.start()


@pytest.fixture
def start_server(builder):
    """Starts a server with extra environment variables for one test."""
    started: list[Server] = []

    def start(**env: str) -> Server:
        srv = builder.start(**env)
        started.append(srv)
        return srv

    yield start
    for srv in started:
        builder.stop(srv)
        builder.servers.remove(srv)


@pytest.fixture
def client(server, tmp_path) -> Client:
    return Client(server, tmp_path / "hf-home")


def pytest_report_header(config):
    return f"hf_xet {hf_xet_version()}"
