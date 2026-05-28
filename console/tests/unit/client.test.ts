import { describe, expect, it, vi, beforeEach, afterEach } from "vitest";
import { ApiError, apiFetch } from "../../src/api/client";

describe("apiFetch", () => {
  beforeEach(() => {
    vi.stubGlobal(
      "fetch",
      vi.fn(() =>
        Promise.resolve(
          new Response(JSON.stringify({ items: [], pagination: { page: 1, pageSize: 20, total: 0 } }), {
            status: 200,
            headers: { "Content-Type": "application/json" },
          }),
        ),
      ),
    );
  });
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("parses json on success", async () => {
    const r = await apiFetch<{
      items: unknown[];
      pagination: { page: number; pageSize: number; total: number };
    }>("/sandboxes");
    expect(r.items).toEqual([]);
  });

  it("throws ApiError on 401 with body", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(() =>
        Promise.resolve(
          new Response(JSON.stringify({ code: "MISSING_TRUSTED_IDENTITY", message: "nope" }), {
            status: 401,
            headers: { "Content-Type": "application/json" },
          }),
        ),
      ),
    );
    await expect(apiFetch("/x")).rejects.toMatchObject({ status: 401, body: { code: "MISSING_TRUSTED_IDENTITY" } });
  });

  it("returns null for 204", async () => {
    vi.stubGlobal("fetch", vi.fn(() => Promise.resolve(new Response(null, { status: 204 }))));
    const r = await apiFetch<null>("/sandboxes/abc");
    expect(r).toBeNull();
  });

  it("shows helpful message for 500 without json body", async () => {
    vi.stubGlobal("fetch", vi.fn(() => Promise.resolve(new Response("Internal Server Error", { status: 500 }))));
    await expect(apiFetch("/x")).rejects.toMatchObject({
      status: 500,
      body: { code: "HTTP_ERROR", message: "Internal Server Error" },
    });
  });
});

describe("ApiError", () => {
  it("exposes code and message", () => {
    const e = new ApiError(403, { code: "INSUFFICIENT_ROLE", message: "no" });
    expect(e.status).toBe(403);
    expect(e.body.code).toBe("INSUFFICIENT_ROLE");
  });
});
