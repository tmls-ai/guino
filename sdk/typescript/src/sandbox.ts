import type {
  SandboxConfig,
  SandboxInfo,
  ExecResult,
  ExecOptions,
  FileInfo,
  SnapshotInfo,
  SandboxStats,
  ApiErrorResponse,
} from "./types.js";

/** Error thrown when an API request fails. */
export class GuinoError extends Error {
  constructor(
    public readonly statusCode: number,
    message: string,
  ) {
    super(message);
    this.name = "GuinoError";
  }
}

/** Internal HTTP helper shared by manager and sandbox instances. */
class HttpClient {
  constructor(
    private readonly baseUrl: string,
    private readonly headers: Record<string, string>,
  ) {}

  async request<T>(
    method: string,
    path: string,
    body?: unknown,
  ): Promise<T> {
    const url = `${this.baseUrl}${path}`;
    const init: RequestInit = {
      method,
      headers: { ...this.headers },
    };

    if (body !== undefined) {
      (init.headers as Record<string, string>)["Content-Type"] =
        "application/json";
      init.body = JSON.stringify(body);
    }

    const response = await fetch(url, init);

    if (!response.ok) {
      let message = `API error (${response.status})`;
      try {
        const errBody = (await response.json()) as ApiErrorResponse;
        if (errBody.error) {
          message = `API error (${response.status}): ${errBody.error}`;
        }
      } catch {
        // If we cannot parse JSON, use the status text
        message = `API error (${response.status}): ${response.statusText}`;
      }
      throw new GuinoError(response.status, message);
    }

    return response.json() as Promise<T>;
  }

  async requestRaw(method: string, path: string): Promise<ArrayBuffer> {
    const url = `${this.baseUrl}${path}`;
    const response = await fetch(url, {
      method,
      headers: { ...this.headers },
    });

    if (!response.ok) {
      let message = `API error (${response.status})`;
      try {
        const errBody = (await response.json()) as ApiErrorResponse;
        if (errBody.error) {
          message = `API error (${response.status}): ${errBody.error}`;
        }
      } catch {
        message = `API error (${response.status}): ${response.statusText}`;
      }
      throw new GuinoError(response.status, message);
    }

    return response.arrayBuffer();
  }

  async requestRawBody(
    method: string,
    path: string,
    body: string | Uint8Array,
  ): Promise<void> {
    const url = `${this.baseUrl}${path}`;
    const response = await fetch(url, {
      method,
      headers: {
        ...this.headers,
        "Content-Type": "application/octet-stream",
      },
      // Both arms are valid BodyInit at runtime (Bun/Node fetch). The cast
      // works around TS 5.7's generic Uint8Array<ArrayBufferLike> not being
      // assignable to the lib's BodyInit under this tsconfig (no @types/node /
      // DOM lib). Web-standard and dependency-free — no Node Buffer.
      body: body as BodyInit,
    });

    if (!response.ok) {
      let message = `API error (${response.status})`;
      try {
        const errBody = (await response.json()) as ApiErrorResponse;
        if (errBody.error) {
          message = `API error (${response.status}): ${errBody.error}`;
        }
      } catch {
        message = `API error (${response.status}): ${response.statusText}`;
      }
      throw new GuinoError(response.status, message);
    }
  }

  async requestNoContent(method: string, path: string): Promise<void> {
    const url = `${this.baseUrl}${path}`;
    const response = await fetch(url, {
      method,
      headers: { ...this.headers },
    });

    if (!response.ok) {
      let message = `API error (${response.status})`;
      try {
        const errBody = (await response.json()) as ApiErrorResponse;
        if (errBody.error) {
          message = `API error (${response.status}): ${errBody.error}`;
        }
      } catch {
        message = `API error (${response.status}): ${response.statusText}`;
      }
      throw new GuinoError(response.status, message);
    }
  }
}

/**
 * Represents an active sandbox with methods to interact with it.
 * Obtained via `SandboxManager.create()` or `SandboxManager.get()`.
 */
export class Sandbox {
  private readonly http: HttpClient;

  /** Unique identifier of this sandbox. */
  readonly id: string;

  /** Docker image used by this sandbox. */
  readonly image: string;

  /** Current status of this sandbox. */
  readonly status: string;

  /** When this sandbox was created. */
  readonly createdAt: string;

  constructor(info: SandboxInfo, http: HttpClient) {
    this.id = info.id;
    this.image = info.image;
    this.status = info.status;
    this.createdAt = info.created_at;
    this.http = http;
  }

  /**
   * Execute a command inside the sandbox.
   * @param cmd - Command and arguments as an array of strings.
   * @param options - Optional execution options (env, workdir, timeout).
   * @returns The execution result with exit code, stdout, and stderr.
   */
  async exec(cmd: string[], options?: ExecOptions): Promise<ExecResult> {
    const body: Record<string, unknown> = { cmd };
    if (options?.env) body.env = options.env;
    if (options?.workdir) body.workdir = options.workdir;
    if (options?.timeout) body.timeout = options.timeout;

    return this.http.request<ExecResult>(
      "POST",
      `/api/v1/sandboxes/${this.id}/exec`,
      body,
    );
  }

  /**
   * Read a file from the sandbox.
   * @param path - Absolute path to the file inside the sandbox.
   * @returns The file content as a string.
   */
  async readFile(path: string): Promise<string> {
    const encodedPath = encodeURIComponent(path);
    const buffer = await this.http.requestRaw(
      "GET",
      `/api/v1/sandboxes/${this.id}/files?path=${encodedPath}`,
    );
    return new TextDecoder().decode(buffer);
  }

  /**
   * Write a file to the sandbox.
   * @param path - Absolute path where the file should be written.
   * @param content - Content to write to the file.
   */
  async writeFile(path: string, content: string | Uint8Array): Promise<void> {
    const encodedPath = encodeURIComponent(path);
    await this.http.requestRawBody(
      "PUT",
      `/api/v1/sandboxes/${this.id}/files?path=${encodedPath}`,
      content,
    );
  }

  /**
   * List files in a directory inside the sandbox.
   * @param path - Absolute path to the directory (defaults to "/").
   * @returns Array of file metadata.
   */
  async listFiles(path = "/"): Promise<FileInfo[]> {
    const encodedPath = encodeURIComponent(path);
    return this.http.request<FileInfo[]>(
      "GET",
      `/api/v1/sandboxes/${this.id}/files/list?path=${encodedPath}`,
    );
  }

  /**
   * Create a directory inside the sandbox.
   * @param path - Absolute path of the directory to create.
   */
  async mkdir(path: string): Promise<void> {
    const encodedPath = encodeURIComponent(path);
    await this.http.requestNoContent(
      "POST",
      `/api/v1/sandboxes/${this.id}/files/mkdir?path=${encodedPath}`,
    );
  }

  /**
   * Remove a file or directory from the sandbox.
   * @param path - Absolute path to remove.
   */
  async removeFile(path: string): Promise<void> {
    const encodedPath = encodeURIComponent(path);
    await this.http.requestNoContent(
      "DELETE",
      `/api/v1/sandboxes/${this.id}/files?path=${encodedPath}`,
    );
  }

  /**
   * Create a snapshot of the current sandbox state.
   * @param name - Human-readable name for the snapshot.
   * @returns Snapshot metadata.
   */
  async snapshot(name: string): Promise<SnapshotInfo> {
    return this.http.request<SnapshotInfo>(
      "POST",
      `/api/v1/sandboxes/${this.id}/snapshots`,
      { name },
    );
  }

  /**
   * List all snapshots of this sandbox.
   * @returns Array of snapshot metadata.
   */
  async listSnapshots(): Promise<SnapshotInfo[]> {
    return this.http.request<SnapshotInfo[]>(
      "GET",
      `/api/v1/sandboxes/${this.id}/snapshots`,
    );
  }

  /**
   * Get resource usage statistics for this sandbox.
   * @returns Current resource usage stats.
   */
  async stats(): Promise<SandboxStats> {
    return this.http.request<SandboxStats>(
      "GET",
      `/api/v1/sandboxes/${this.id}/stats`,
    );
  }

  /**
   * Stop this sandbox (keeps it around for restart).
   */
  async stop(): Promise<void> {
    await this.http.request(
      "POST",
      `/api/v1/sandboxes/${this.id}/stop`,
    );
  }

  /**
   * Permanently destroy this sandbox.
   */
  async destroy(): Promise<void> {
    await this.http.requestNoContent(
      "DELETE",
      `/api/v1/sandboxes/${this.id}`,
    );
  }
}

/**
 * Manages sandbox lifecycle operations.
 * Access via `Guino.sandbox`.
 */
export class SandboxManager {
  /** @internal */
  readonly http: HttpClient;

  /**
   * Server capability tokens, lazily fetched and cached on the first
   * successful probe (null until then so a transient failure is retried).
   * A missing token means "unsupported" — this is a capability hint only,
   * NOT an authentication or authorization signal.
   */
  private features: Set<string> | null = null;

  constructor(baseUrl: string, headers: Record<string, string>) {
    this.http = new HttpClient(baseUrl, headers);
  }

  /**
   * Fail fast if the server does not advertise `feature`. Lazy and scoped:
   * only invoked when the corresponding option is actually used, so servers
   * that predate a feature keep working for everything else.
   */
  private async requireFeature(feature: string): Promise<void> {
    if (this.features === null) {
      const v = await this.http.request<{ features?: string[] }>(
        "GET",
        "/api/v1/version",
      );
      this.features = new Set(v.features ?? []);
    }
    if (!this.features.has(feature)) {
      throw new GuinoError(
        0,
        `server does not advertise the "${feature}" feature; upgrade Guino or omit network_mode`,
      );
    }
  }

  /**
   * Create a new sandbox.
   * @param config - Sandbox configuration including image, env, resources, etc.
   * @returns A Sandbox instance ready for interaction.
   */
  async create(config: SandboxConfig): Promise<Sandbox> {
    if (config.network_mode) {
      await this.requireFeature("network_mode");
    }
    const info = await this.http.request<SandboxInfo>(
      "POST",
      "/api/v1/sandboxes",
      config,
    );
    return new Sandbox(info, this.http);
  }

  /**
   * List all sandboxes.
   * @returns Array of Sandbox instances.
   */
  async list(): Promise<Sandbox[]> {
    const infos = await this.http.request<SandboxInfo[]>(
      "GET",
      "/api/v1/sandboxes",
    );
    return infos.map((info) => new Sandbox(info, this.http));
  }

  /**
   * Get a sandbox by ID.
   * @param id - Sandbox ID.
   * @returns A Sandbox instance.
   */
  async get(id: string): Promise<Sandbox> {
    const info = await this.http.request<SandboxInfo>(
      "GET",
      `/api/v1/sandboxes/${id}`,
    );
    return new Sandbox(info, this.http);
  }

  /**
   * Destroy a sandbox by ID.
   * @param id - Sandbox ID to destroy.
   */
  async destroy(id: string): Promise<void> {
    await this.http.requestNoContent(
      "DELETE",
      `/api/v1/sandboxes/${id}`,
    );
  }

  /**
   * Restore a sandbox from a snapshot.
   * @param snapshotId - Snapshot ID to restore from.
   * @returns A new Sandbox instance created from the snapshot.
   */
  async restoreSnapshot(snapshotId: string): Promise<Sandbox> {
    const result = await this.http.request<{ id: string; status: string }>(
      "POST",
      `/api/v1/snapshots/${snapshotId}/restore`,
    );
    return this.get(result.id);
  }

  /**
   * Delete a snapshot.
   * @param snapshotId - Snapshot ID to delete.
   */
  async deleteSnapshot(snapshotId: string): Promise<void> {
    await this.http.requestNoContent(
      "DELETE",
      `/api/v1/snapshots/${snapshotId}`,
    );
  }
}

/** @deprecated Use GuinoError. This alias is retained for the 0.1.x transition. */
export { GuinoError as DenError };
