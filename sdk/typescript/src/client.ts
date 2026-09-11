import type { ClientConfig } from "./types.js";
import { SandboxManager } from "./sandbox.js";

/**
 * Main entry point for the Guino SDK.
 *
 * @example
 * ```ts
 * const client = new Guino({ url: "http://localhost:8080", apiKey: "my-key" });
 * const sandbox = await client.sandbox.create({ image: "ubuntu:22.04" });
 * const result = await sandbox.exec(["echo", "hello"]);
 * console.log(result.stdout); // "hello\n"
 * await sandbox.destroy();
 * ```
 */
export class Guino {
  private readonly _sandbox: SandboxManager;
  private readonly baseUrl: string;
  private readonly headers: Record<string, string>;

  constructor(config: ClientConfig) {
    // Remove only the trailing slash suffix, without regex backtracking.
    let end = config.url.length;
    while (end > 0 && config.url[end - 1] === "/") end--;
    this.baseUrl = config.url.slice(0, end);
    this.headers = {};

    if (config.apiKey) {
      this.headers["X-API-Key"] = config.apiKey;
    }

    this._sandbox = new SandboxManager(this.baseUrl, this.headers);
  }

  /** Access sandbox management operations. */
  get sandbox(): SandboxManager {
    return this._sandbox;
  }

  /**
   * Check if the Guino server is healthy.
   * @returns True if the server is reachable and healthy.
   */
  async health(): Promise<boolean> {
    try {
      const response = await fetch(`${this.baseUrl}/api/v1/health`, {
        headers: this.headers,
      });
      if (!response.ok) return false;
      const body = (await response.json()) as { status: string };
      return body.status === "ok";
    } catch {
      return false;
    }
  }

  /**
   * Get the server version information.
   * @returns Version, commit hash, build date, and the server's optional
   * capability tokens. `features` is a capability hint only (NOT auth); it is
   * absent on servers that predate it — treat a missing token as "unsupported".
   */
  async version(): Promise<{
    version: string;
    commit: string;
    build_date: string;
    features?: string[];
  }> {
    const response = await fetch(`${this.baseUrl}/api/v1/version`, {
      headers: this.headers,
    });
    if (!response.ok) {
      throw new Error(`Failed to fetch version: ${response.status}`);
    }
    return response.json() as Promise<{
      version: string;
      commit: string;
      build_date: string;
      features?: string[];
    }>;
  }
}

/** @deprecated Use Guino. This alias is retained for the 0.1.x transition. */
export { Guino as Den };
