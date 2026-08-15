import { getMigrations } from "better-auth/db/migration";
import { auth } from "../lib/auth";

async function main() {
  const migrations = await getMigrations(auth.options);
  await migrations.runMigrations();
  process.stdout.write("dashboard authentication schema is current\n");
}

main().catch((error: unknown) => {
  const message = error instanceof Error ? error.message : "unknown error";
  console.error(`dashboard authentication migration failed: ${message}`);
  process.exitCode = 1;
});
