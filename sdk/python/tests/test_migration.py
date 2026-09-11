"""Public imports, package metadata, and HTTP compatibility across the rename."""

import importlib
from importlib.metadata import version

import pytest
import respx

import den
import guino
from guino import Den, DenError, Guino, GuinoError


def test_legacy_names_preserve_identity():
    assert Den is Guino
    assert den.Den is Guino
    assert DenError is GuinoError
    assert den.DenError is GuinoError
    assert importlib.import_module("den.sandbox").DenError is GuinoError
    assert isinstance(guino.AuthenticationError(), den.DenError)
    for name in guino.__all__:
        assert getattr(den, name) is getattr(guino, name)


@pytest.mark.parametrize("module", ["client", "exceptions", "sandbox", "types"])
def test_legacy_submodule_exports_preserve_identity(module):
    current = importlib.import_module(f"guino.{module}")
    legacy = importlib.import_module(f"den.{module}")
    for name, value in vars(current).items():
        if not name.startswith("_") and isinstance(value, type):
            assert getattr(legacy, name) is value


def test_installed_distribution_matches_runtime_version():
    assert version("guino") == guino.__version__ == den.__version__


@pytest.mark.parametrize("client_type", [Guino, den.Den], ids=["guino", "den"])
@respx.mock
def test_sync_http_paths_and_auth_unchanged(client_type):
    health = respx.get("http://guino.test/api/v1/health").respond(200, json={"status": "ok"})
    version_route = respx.get("http://guino.test/api/v1/version").respond(
        200, json={"version": "0.1.0", "features": ["network_mode"]}
    )
    missing = respx.get("http://guino.test/api/v1/sandboxes/missing").respond(
        404, json={"error": "missing sandbox"}
    )
    with client_type("http://guino.test///", api_key="test-key") as client:
        assert client.health() == {"status": "ok"}
        assert client.version()["features"] == ["network_mode"]
        with pytest.raises(GuinoError, match="missing sandbox") as error:
            client.sandbox.get("missing")
        assert isinstance(error.value, den.DenError)
        assert error.value.status_code == 404
    for route in (health, version_route, missing):
        assert route.call_count == 1
        assert route.calls[0].request.headers["X-API-Key"] == "test-key"


@pytest.mark.parametrize("client_type", [Guino, den.Den], ids=["guino", "den"])
@respx.mock
async def test_async_http_paths_and_auth_unchanged(client_type):
    health = respx.get("http://guino.test/api/v1/health").respond(200, json={"status": "ok"})
    version_route = respx.get("http://guino.test/api/v1/version").respond(
        200, json={"version": "0.1.0", "features": ["network_mode"]}
    )
    client = client_type("http://guino.test///", api_key="test-key")
    try:
        async with client:
            assert await client.ahealth() == {"status": "ok"}
            assert (await client.aversion())["features"] == ["network_mode"]
    finally:
        client.close()
    for route in (health, version_route):
        assert route.call_count == 1
        assert route.calls[0].request.headers["X-API-Key"] == "test-key"
