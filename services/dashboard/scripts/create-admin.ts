import { stdin, stdout } from "node:process";
import { auth } from "../lib/auth";

async function main() {
  let payload = "";
  for await (const chunk of stdin) payload += chunk.toString();
  const [rawEmail = "", password = ""] = payload.split(/\r?\n/, 2);
  const email = rawEmail.trim().toLowerCase();
  if (!/^[^@\s]+@[^@\s]+\.[^@\s]+$/.test(email)) throw new Error("a valid email is required");
  if (password.length < 9 || password.length > 128) throw new Error("password must contain 9 to 128 characters");
  await auth.api.createUser({ body: { email, password, name: email, role: "admin" } });
  stdout.write("dashboard administrator created\n");
}

main().catch((error: unknown) => {
  const message = error instanceof Error ? error.message : "unknown error";
  console.error(`dashboard administrator was not created: ${message}`);
  process.exitCode = 1;
});
