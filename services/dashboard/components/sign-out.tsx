"use client";

import { useRouter } from "next/navigation";
import { authClient } from "@/lib/auth-client";

export function SignOut() {
  const router = useRouter();
  return <button className="avatar-button" onClick={async () => { await authClient.signOut(); router.replace("/sign-in"); router.refresh(); }} aria-label="Sign out">↗</button>;
}
