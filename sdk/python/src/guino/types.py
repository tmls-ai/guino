"""Pydantic models for the Guino API."""

from __future__ import annotations

from datetime import datetime
from typing import Literal

from pydantic import BaseModel, Field


class PortMapping(BaseModel):
    """Defines a port forwarding between host and sandbox."""

    sandbox_port: int
    host_port: int
    # Always "tcp"; Guino does not support udp port publishing.
    protocol: str = "tcp"


class VolumeMount(BaseModel):
    """A named Docker volume to mount into a sandbox."""

    name: str
    mount_path: str
    read_only: bool = False


class TmpfsMount(BaseModel):
    """A tmpfs filesystem to mount inside a sandbox."""

    path: str
    size: str  # "256m", "1g"
    options: str | None = None  # "rw,noexec,nosuid"


class S3SyncConfig(BaseModel):
    """S3 synchronization configuration for a sandbox."""

    endpoint: str | None = None
    bucket: str
    prefix: str | None = None
    region: str | None = None
    access_key: str | None = None
    secret_key: str | None = None
    mode: Literal["hooks", "fuse", "on_demand"]
    mount_path: str | None = None  # FUSE mode
    sync_path: str | None = None  # Hooks mode


class StorageConfig(BaseModel):
    """Storage configuration for a sandbox."""

    volumes: list[VolumeMount] | None = None
    tmpfs: list[TmpfsMount] | None = None
    s3: S3SyncConfig | None = None


class SandboxConfig(BaseModel):
    """Configuration for creating a new sandbox."""

    image: str = ""
    env: dict[str, str] | None = None
    workdir: str | None = None
    timeout: int | None = None  # seconds
    cpu: int | None = None  # NanoCPUs (1e9 = 1 core)
    memory: int | None = None  # bytes
    ports: list[PortMapping] | None = None
    storage: StorageConfig | None = None
    # Opt out of the read-only rootfs default (secure default is read-only).
    # Needed for heavyweight images that write outside tmpfs/volume mounts.
    writable_rootfs: bool | None = None
    # Per-sandbox network override. Only "" (inherit the server default) or
    # "none" (no network at all) are accepted; any other value -- including
    # one equal to the server default -- is rejected with HTTP 400. A
    # per-sandbox value may only INCREASE isolation, never decrease it.
    network_mode: Literal["", "none"] | None = None


class SandboxInfo(BaseModel):
    """Information about a sandbox instance."""

    id: str
    image: str = ""
    status: str = ""
    created_at: datetime | None = None
    expires_at: datetime | None = None
    ports: list[PortMapping] | None = None
    # Container IP on the managed bridge network. Reachable for any port from a
    # co-resident Linux host; empty for network_mode="none" or when the bridge
    # is not host-routable (e.g. Docker Desktop on macOS).
    ip: str = ""


class ExecResult(BaseModel):
    """Result of a command execution inside a sandbox."""

    exit_code: int
    stdout: str = ""
    stderr: str = ""


class FileInfo(BaseModel):
    """Metadata about a file inside a sandbox."""

    name: str
    path: str = ""
    size: int = 0
    mode: str = ""
    mod_time: datetime | None = None
    is_dir: bool = False


class SnapshotInfo(BaseModel):
    """Metadata about a sandbox snapshot."""

    id: str
    sandbox_id: str = ""
    name: str = ""
    image_id: str = ""
    created_at: datetime | None = None
    size: int = 0


class SandboxStats(BaseModel):
    """Resource usage statistics for a sandbox."""

    cpu_percent: float = 0.0
    memory_usage: int = 0  # bytes
    memory_limit: int = 0  # bytes
    memory_percent: float = 0.0
    network_rx: int = 0  # bytes
    network_tx: int = 0  # bytes
    disk_read: int = 0  # bytes
    disk_write: int = 0  # bytes
    pid_count: int = 0
    timestamp: datetime | None = None


class ExecOpts(BaseModel):
    """Options for executing a command in a sandbox."""

    cmd: list[str]
    env: dict[str, str] | None = None
    workdir: str | None = None
    timeout: int | None = None  # seconds


class S3ImportRequest(BaseModel):
    """Request body for S3 import operation."""

    bucket: str
    key: str
    dest_path: str
    endpoint: str | None = None
    access_key: str | None = None
    secret_key: str | None = None
    region: str | None = None


class S3ImportResponse(BaseModel):
    """Response from an S3 import operation."""

    success: bool
    bytes_downloaded: int
    path: str


class S3ExportRequest(BaseModel):
    """Request body for S3 export operation."""

    source_path: str
    bucket: str
    key: str
    endpoint: str | None = None
    access_key: str | None = None
    secret_key: str | None = None
    region: str | None = None


class S3ExportResponse(BaseModel):
    """Response from an S3 export operation."""

    success: bool
    bytes_uploaded: int
    s3_key: str


class ErrorResponse(BaseModel):
    """API error response."""

    error: str = Field(default="")
