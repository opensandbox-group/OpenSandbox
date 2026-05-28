import { describe, expect, it, vi, afterEach } from "vitest";
import { canMutate, parseRoleFromEnv } from "../../src/api/role";

describe("parseRoleFromEnv", () => {
  afterEach(() => {
    vi.unstubAllEnvs();
  });
  it("defaults to operator", () => {
    vi.stubEnv("VITE_UI_ROLE", undefined);
    expect(parseRoleFromEnv()).toBe("operator");
  });
  it("detects read_only", () => {
    vi.stubEnv("VITE_UI_ROLE", "read_only");
    expect(parseRoleFromEnv()).toBe("read_only");
  });
});

describe("canMutate", () => {
  it("operator can mutate", () => {
    expect(canMutate("operator")).toBe(true);
  });
  it("read_only cannot", () => {
    expect(canMutate("read_only")).toBe(false);
  });
});
