"""Main client for the Guino API."""

from __future__ import annotations

import httpx

from guino.sandbox import SandboxManager


class Guino:
    """Client for the Guino sandbox API.

    Provides access to sandbox management and operations via both
    synchronous and asynchronous interfaces.

    Example (sync)::

        client = Guino("http://localhost:8080", api_key="sk-...")
        sandbox = client.sandbox.create(image="python:3.12")
        result = sandbox.exec(["python", "-c", "print('hello')"])
        print(result.stdout)
        sandbox.destroy()
        client.close()

    Example (async)::

        client = Guino("http://localhost:8080", api_key="sk-...")
        sandbox = await client.sandbox.acreate(image="python:3.12")
        result = await sandbox.aexec(["python", "-c", "print('hello')"])
        print(result.stdout)
        await sandbox.adestroy()
        await client.aclose()
    """

    def __init__(
        self,
        url: str = "http://localhost:8080",
        *,
        api_key: str | None = None,
        timeout: float = 120.0,
    ) -> None:
        """Initialize the Guino client.

        Args:
            url: Base URL of the Guino server (e.g. ``http://localhost:8080``).
            api_key: Optional API key for authentication.
            timeout: Request timeout in seconds. Defaults to 120.
        """
        self._url = url.rstrip("/")
        self._api_key = api_key
        self._base_url = f"{self._url}/api/v1"

        headers: dict[str, str] = {}
        if api_key:
            headers["X-API-Key"] = api_key

        self._client = httpx.Client(
            base_url="",
            headers=headers,
            timeout=timeout,
        )
        self._async_client = httpx.AsyncClient(
            base_url="",
            headers=headers,
            timeout=timeout,
        )

        self._sandbox_manager = SandboxManager(
            client=self._client,
            async_client=self._async_client,
            base_url=self._base_url,
        )

    @property
    def sandbox(self) -> SandboxManager:
        """Access the sandbox manager for creating and managing sandboxes."""
        return self._sandbox_manager

    def health(self) -> dict:
        """Check server health (sync).

        Returns:
            Dictionary with server health status.
        """
        resp = self._client.get(f"{self._base_url}/health")
        resp.raise_for_status()
        return resp.json()

    async def ahealth(self) -> dict:
        """Check server health (async).

        Returns:
            Dictionary with server health status.
        """
        resp = await self._async_client.get(f"{self._base_url}/health")
        resp.raise_for_status()
        return resp.json()

    def version(self) -> dict:
        """Get server version info (sync).

        Returns:
            Dictionary with ``version``, ``commit``, ``build_date``, and
            ``features`` (a list of server capability tokens). ``features``
            is a capability hint only — NOT an auth signal — and is absent on
            servers that predate it; treat a missing token as "unsupported".
        """
        resp = self._client.get(f"{self._base_url}/version")
        resp.raise_for_status()
        return resp.json()

    async def aversion(self) -> dict:
        """Get server version info (async).

        Returns:
            Dictionary with ``version``, ``commit``, ``build_date``, and
            ``features`` (a list of server capability tokens). ``features``
            is a capability hint only — NOT an auth signal — and is absent on
            servers that predate it; treat a missing token as "unsupported".
        """
        resp = await self._async_client.get(f"{self._base_url}/version")
        resp.raise_for_status()
        return resp.json()

    def close(self) -> None:
        """Close the underlying HTTP clients (sync).

        Note: The async client is best closed via ``await client.aclose()``
        or by using ``async with Guino(...)`` as a context manager.
        This method attempts to close it as well, but in an async context
        prefer calling ``aclose()`` directly.
        """
        self._client.close()
        try:
            import asyncio

            try:
                loop = asyncio.get_running_loop()
                # Loop is running — schedule async close, log failures
                task = loop.create_task(self._async_client.aclose())
                task.add_done_callback(lambda t: None if t.exception() is None else None)
            except RuntimeError:
                # No running loop — safe to run synchronously
                asyncio.run(self._async_client.aclose())
        except Exception:
            import warnings

            warnings.warn(
                "Failed to close async HTTP client; prefer using "
                "'await client.aclose()' in async contexts",
                ResourceWarning,
                stacklevel=2,
            )

    async def aclose(self) -> None:
        """Close the underlying HTTP clients (async)."""
        await self._async_client.aclose()

    def __enter__(self) -> Guino:
        return self

    def __exit__(self, *args: object) -> None:
        self.close()

    async def __aenter__(self) -> Guino:
        return self

    async def __aexit__(self, *args: object) -> None:
        await self.aclose()

    def __repr__(self) -> str:
        return f"Guino(url={self._url!r})"


# Deprecated alias retained for the 0.1.x transition.
Den = Guino
