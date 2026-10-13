"""Checks the pytest launcher itself."""

import requests

import hangar_instance


def test_instance_serves_health(hangar):
    resp = requests.get(f"{hangar.url}/healthz", timeout=5)
    assert resp.status_code == 200
    assert resp.json() == {"status": "ok"}
    assert resp.headers["X-Request-Id"]


def test_instances_are_isolated_and_stop():
    inst = hangar_instance.start()
    try:
        assert requests.get(f"{inst.url}/readyz", timeout=5).status_code == 200
    finally:
        hangar_instance.cleanup(inst)
    assert inst.process.poll() is not None
