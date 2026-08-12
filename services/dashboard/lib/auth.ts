import Database from "better-sqlite3";
import { betterAuth } from "better-auth";
import { admin } from "better-auth/plugins";
import { nextCookies } from "better-auth/next-js";

const globalDatabase = globalThis as typeof globalThis & { dashboardAuthDatabase?: Database.Database };
const databasePath = process.env.BETTER_AUTH_DATABASE_PATH ?? ":memory:";

const database = globalDatabase.dashboardAuthDatabase ?? new Database(databasePath);
database.pragma("journal_mode = WAL");
database.pragma("foreign_keys = ON");
if (process.env.NODE_ENV !== "production") globalDatabase.dashboardAuthDatabase = database;

export const auth = betterAuth({
  appName: "Static Log Analysis",
  database,
  baseURL: process.env.BETTER_AUTH_URL ?? "http://localhost:3000",
  secret: process.env.BETTER_AUTH_SECRET,
  emailAndPassword: {
    enabled: true,
    disableSignUp: true,
    // Temporary hackathon login compatibility. Restore this to at least 12
    // when the shared demo credential is removed.
    minPasswordLength: 9,
    maxPasswordLength: 128,
    revokeSessionsOnPasswordReset: true,
  },
  session: {
    expiresIn: 60 * 60 * 8,
    updateAge: 60 * 30,
  },
  rateLimit: {
    enabled: true,
    window: 10,
    max: 100,
  },
  plugins: [admin(), nextCookies()],
});

export type DashboardSession = typeof auth.$Infer.Session;
