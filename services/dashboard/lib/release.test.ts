import { afterEach, describe, expect, it } from "vitest";
import { getReleaseIdentity } from "./release";

afterEach(() => {
  delete process.env.SLA_RELEASE_SHA;
});

describe("dashboard release identity", () => {
  it("shows only an immutable full commit SHA", () => {
    process.env.SLA_RELEASE_SHA = "0123456789abcdef0123456789abcdef01234567";

    expect(getReleaseIdentity()).toEqual({
      commit: "0123456789abcdef0123456789abcdef01234567",
      shortCommit: "0123456789ab",
    });
  });

  it.each(["main", "0123456", "0123456789ABCDEF0123456789ABCDEF01234567", "not a ref"])(
    "does not present a mutable or malformed release ref as identity: %s",
    (value) => {
      process.env.SLA_RELEASE_SHA = value;
      expect(getReleaseIdentity()).toBeNull();
    },
  );
});
