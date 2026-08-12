import { describe, expect, it } from "vitest";
import { loginEmail } from "./login-identity";

describe("temporary dashboard login identity", () => {
  it("maps the temporary admin username to its private auth identity", () => {
    expect(loginEmail("admin")).toBe("admin@static-log-analysis.local");
    expect(loginEmail(" ADMIN ")).toBe("admin@static-log-analysis.local");
  });

  it("preserves provisioned email identities", () => {
    expect(loginEmail("operator@example.com")).toBe("operator@example.com");
  });
});
