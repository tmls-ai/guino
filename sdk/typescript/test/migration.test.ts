import { afterEach, describe, expect, test } from "bun:test";

import { Den, DenError, Guino, GuinoError } from "../src/index.ts";

const realFetch = globalThis.fetch;
afterEach(() => {
  globalThis.fetch = realFetch;
});

describe("Guino SDK migration", () => {
  test("legacy clients and errors retain constructor identity", () => {
    expect(Den).toBe(Guino);
    expect(DenError).toBe(GuinoError);
    expect(new Den({ url: "http://localhost:8080" })).toBeInstanceOf(Guino);
    const error = new GuinoError(401, "unauthorized");
    expect(error).toBeInstanceOf(DenError);
    expect(error.name).toBe("GuinoError");
    expect(error.statusCode).toBe(401);
  });

  test("URL trimming preserves empty URLs, paths, and long internal slash runs", async () => {
    const interiorSlashes = "/".repeat(100_000);
    const cases = [
      ["", ""],
      ["/", ""],
      ["///", ""],
      ["http://guino.test", "http://guino.test"],
      ["http://guino.test/api///", "http://guino.test/api"],
      ["http://guino.test/path//?query=x", "http://guino.test/path//?query=x"],
      ["http://guino.test/path///\n", "http://guino.test/path///\n"],
      [`http://guino.test${interiorSlashes}tail`, `http://guino.test${interiorSlashes}tail`],
      [`http://guino.test${interiorSlashes}`, "http://guino.test"],
    ] as const;
    let requestedUrl: string | undefined;
    globalThis.fetch = (async (input: RequestInfo | URL) => {
      requestedUrl = String(input);
      return Response.json({ status: "ok" });
    }) as typeof fetch;

    for (const [url, expectedBase] of cases) {
      expect(await new Guino({ url }).health()).toBe(true);
      expect(requestedUrl).toBe(`${expectedBase}/api/v1/health`);
    }
  });

  for (const [name, Client] of [["Guino", Guino], ["Den", Den]] as const) {
    test(`${name} retain HTTP paths and auth`, async () => {
      const hits: { url: string; apiKey: string | null }[] = [];
      globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
        const url = String(input);
        hits.push({ url, apiKey: new Headers(init?.headers).get("X-API-Key") });
        if (url.endsWith("/health")) {
          return Response.json({ status: "ok" });
        }
        if (url.endsWith("/version")) {
          return Response.json({ version: "0.1.0", features: ["network_mode"] });
        }
        return Response.json({ error: "missing sandbox" }, { status: 404 });
      }) as typeof fetch;

      const client = new Client({ url: "http://localhost:8080///", apiKey: "test-key" });
      expect(await client.health()).toBe(true);
      expect((await client.version()).features).toEqual(["network_mode"]);
      await expect(client.sandbox.get("missing")).rejects.toBeInstanceOf(GuinoError);
      expect(hits).toEqual([
        { url: "http://localhost:8080/api/v1/health", apiKey: "test-key" },
        { url: "http://localhost:8080/api/v1/version", apiKey: "test-key" },
        { url: "http://localhost:8080/api/v1/sandboxes/missing", apiKey: "test-key" },
      ]);
    });
  }
});
