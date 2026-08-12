import { describe, expect, it } from "vitest";
import { canViewDashboard } from "./authorization";

describe("dashboard database authorization", () => {
	it("fails closed without a valid session", () => {
		expect(canViewDashboard(null)).toBe(false);
	});

	it("permits only an unbanned admin", () => {
		expect(canViewDashboard({ user: { role: "admin", banned: false } })).toBe(true);
		expect(canViewDashboard({ user: { role: "user", banned: false } })).toBe(false);
	});

	it("rejects a banned admin and handles multiple roles", () => {
		expect(canViewDashboard({ user: { role: "admin", banned: true } })).toBe(false);
		expect(canViewDashboard({ user: { role: "user,admin", banned: false } })).toBe(true);
	});
});
