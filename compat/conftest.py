"""Shared pytest fixtures for every compat suite."""

from collections.abc import Iterator

import pytest

import hangar_instance


@pytest.fixture(scope="session")
def hangar() -> Iterator[hangar_instance.Instance]:
    """A Hangar Instance shared by the whole test session."""
    inst = hangar_instance.start()
    yield inst
    hangar_instance.cleanup(inst)


@pytest.fixture
def hf_endpoint(hangar, monkeypatch) -> str:
    """Points huggingface_hub at the Instance for one test."""
    monkeypatch.setenv("HF_ENDPOINT", hangar.url)
    monkeypatch.setenv("HF_HUB_DISABLE_TELEMETRY", "1")
    return hangar.url
