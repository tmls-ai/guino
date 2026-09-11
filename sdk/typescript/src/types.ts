/** A named Docker volume to mount into a sandbox. */
export interface VolumeMount {
  /** Volume name (alphanumeric, dots, dashes, underscores). */
  name: string;
  /** Absolute path inside the sandbox to mount the volume. */
  mount_path: string;
  /** Whether the volume should be mounted read-only. */
  read_only?: boolean;
}

/** A tmpfs filesystem to mount inside a sandbox. */
export interface TmpfsMount {
  /** Absolute path inside the sandbox. */
  path: string;
  /** Size string (e.g. "256m", "1g"). */
  size: string;
  /** Mount options (e.g. "rw,noexec,nosuid"). */
  options?: string;
}

/** S3 synchronization mode. */
export type S3SyncMode = "hooks" | "fuse" | "on_demand";

/** S3 synchronization configuration for a sandbox. */
export interface S3SyncConfig {
  /** S3-compatible endpoint URL (optional, falls back to server config). */
  endpoint?: string;
  /** S3 bucket name. */
  bucket: string;
  /** Key prefix for S3 objects. */
  prefix?: string;
  /** AWS region. */
  region?: string;
  /** Access key ID (optional, falls back to server config). */
  access_key?: string;
  /** Secret access key (optional, falls back to server config). */
  secret_key?: string;
  /** Synchronization mode. */
  mode: S3SyncMode;
  /** FUSE mount path inside the sandbox. */
  mount_path?: string;
  /** Local path to sync (hooks mode). */
  sync_path?: string;
}

/** Storage configuration for a sandbox. */
export interface StorageConfig {
  /** Named Docker volumes to mount. */
  volumes?: VolumeMount[];
  /** Tmpfs mounts (overrides server defaults). */
  tmpfs?: TmpfsMount[];
  /** S3 synchronization settings. */
  s3?: S3SyncConfig;
}

/** Configuration for creating a new sandbox. */
export interface SandboxConfig {
  /** Docker image to use for the sandbox. */
  image: string;
  /** Environment variables to set inside the sandbox. */
  env?: Record<string, string>;
  /** Working directory inside the sandbox. */
  workdir?: string;
  /** Timeout in seconds before the sandbox expires. */
  timeout?: number;
  /** CPU allocation in NanoCPUs (1e9 = 1 core). */
  cpu?: number;
  /** Memory limit in bytes. */
  memory?: number;
  /** Port mappings to expose from the sandbox. */
  ports?: PortMapping[];
  /** Storage configuration (volumes, tmpfs, S3). */
  storage?: StorageConfig;
  /**
   * Per-sandbox network override. Only "" (inherit the server default) or
   * "none" (no network at all) are accepted; any other value — including one
   * equal to the server default — is rejected with HTTP 400. A per-sandbox
   * value may only INCREASE isolation, never decrease it.
   */
  network_mode?: "" | "none";
}

/** Options for executing a command inside a sandbox. */
export interface ExecOptions {
  /** Environment variables for this execution. */
  env?: Record<string, string>;
  /** Working directory for this execution. */
  workdir?: string;
  /** Timeout in seconds for this execution. */
  timeout?: number;
}

/** The current status of a sandbox. */
export type SandboxStatus = "creating" | "running" | "stopped" | "error";

/** Represents a sandbox instance returned by the API. */
export interface SandboxInfo {
  /** Unique identifier for the sandbox. */
  id: string;
  /** Docker image used by the sandbox. */
  image: string;
  /** Current status of the sandbox. */
  status: SandboxStatus;
  /** When the sandbox was created (ISO 8601). */
  created_at: string;
  /** When the sandbox expires (ISO 8601), if applicable. */
  expires_at?: string;
  /** Port mappings for the sandbox. */
  ports?: PortMapping[];
}

/** Result of a synchronous command execution. */
export interface ExecResult {
  /** Exit code of the command. */
  exit_code: number;
  /** Standard output of the command. */
  stdout: string;
  /** Standard error of the command. */
  stderr: string;
}

/** A single message from a streaming exec session. */
export interface ExecStreamMessage {
  /** Message type: "stdout", "stderr", or "exit". */
  type: "stdout" | "stderr" | "exit";
  /** Message payload. */
  data: string;
}

/** Metadata about a file inside a sandbox. */
export interface FileInfo {
  /** File name. */
  name: string;
  /** Full path to the file. */
  path: string;
  /** File size in bytes. */
  size: number;
  /** File mode as a string (e.g. "0644"). */
  mode: string;
  /** Last modification time (ISO 8601). */
  mod_time: string;
  /** Whether this entry is a directory. */
  is_dir: boolean;
}

/** Metadata about a sandbox snapshot. */
export interface SnapshotInfo {
  /** Unique identifier for the snapshot. */
  id: string;
  /** ID of the sandbox this snapshot was taken from. */
  sandbox_id: string;
  /** Human-readable name for the snapshot. */
  name: string;
  /** Docker image ID for the snapshot. */
  image_id: string;
  /** When the snapshot was created (ISO 8601). */
  created_at: string;
  /** Size of the snapshot in bytes. */
  size: number;
}

/** Resource usage statistics for a sandbox. */
export interface SandboxStats {
  /** CPU usage as a percentage. */
  cpu_percent: number;
  /** Current memory usage in bytes. */
  memory_usage: number;
  /** Memory limit in bytes. */
  memory_limit: number;
  /** Memory usage as a percentage. */
  memory_percent: number;
  /** Network bytes received. */
  network_rx: number;
  /** Network bytes transmitted. */
  network_tx: number;
  /** Disk bytes read. */
  disk_read: number;
  /** Disk bytes written. */
  disk_write: number;
  /** Number of running processes. */
  pid_count: number;
  /** Timestamp of the stats reading (ISO 8601). */
  timestamp: string;
}

/** Port mapping between host and sandbox. */
export interface PortMapping {
  /** Port inside the sandbox. */
  sandbox_port: number;
  /** Port on the host. */
  host_port: number;
  /** Protocol — always "tcp". Guino does not support udp port publishing. */
  protocol?: string;
}

/** Configuration for the Guino client. */
export interface ClientConfig {
  /** Base URL of the Guino server (e.g. "http://localhost:8080"). */
  url: string;
  /** API key for authentication. */
  apiKey?: string;
}

/** Request body for S3 import operation. */
export interface S3ImportRequest {
  /** S3 bucket name. */
  bucket: string;
  /** S3 object key. */
  key: string;
  /** Destination path inside the sandbox. */
  dest_path: string;
  /** S3-compatible endpoint URL (optional, falls back to server config). */
  endpoint?: string;
  /** Access key ID (optional, falls back to server config). */
  access_key?: string;
  /** Secret access key (optional, falls back to server config). */
  secret_key?: string;
  /** AWS region (optional). */
  region?: string;
}

/** Response from an S3 import operation. */
export interface S3ImportResponse {
  success: boolean;
  bytes_downloaded: number;
  path: string;
}

/** Request body for S3 export operation. */
export interface S3ExportRequest {
  /** Source path inside the sandbox. */
  source_path: string;
  /** S3 bucket name. */
  bucket: string;
  /** S3 object key. */
  key: string;
  /** S3-compatible endpoint URL (optional, falls back to server config). */
  endpoint?: string;
  /** Access key ID (optional, falls back to server config). */
  access_key?: string;
  /** Secret access key (optional, falls back to server config). */
  secret_key?: string;
  /** AWS region (optional). */
  region?: string;
}

/** Response from an S3 export operation. */
export interface S3ExportResponse {
  success: boolean;
  bytes_uploaded: number;
  s3_key: string;
}

/** Error response from the API. */
export interface ApiErrorResponse {
  error: string;
}
