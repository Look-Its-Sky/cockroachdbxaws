"use client";

import { useRouter } from "next/navigation";
import { FormEvent, useState } from "react";
import { authClient } from "@/lib/auth-client";
import { loginEmail } from "@/lib/login-identity";

export function SignInForm() {
  const router = useRouter();
  const [error, setError] = useState("");
  const [pending, setPending] = useState(false);

  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setPending(true);
    setError("");
    const values = new FormData(event.currentTarget);
    const result = await authClient.signIn.email({
      email: loginEmail(String(values.get("identity") ?? "")),
      password: String(values.get("password") ?? ""),
      rememberMe: false,
    });
    setPending(false);
    if (result.error) {
      setError("The username or password was not accepted.");
      return;
    }
    router.replace("/");
    router.refresh();
  }

  return (
    <form className="sign-in-card" onSubmit={submit}>
      <div><p className="eyebrow">AUTHORIZED ACCESS</p><h2>Sign in</h2></div>
      <label>Username or email<input name="identity" type="text" autoComplete="username" required /></label>
      <label>Password<input name="password" type="password" autoComplete="current-password" minLength={9} maxLength={128} required /></label>
      {error ? <p className="form-error" role="alert">{error}</p> : null}
      <button className="button" disabled={pending}>{pending ? "Signing in…" : "Sign in"}</button>
      <p className="form-note">Accounts are provisioned by the project operator. Public signup is disabled.</p>
    </form>
  );
}
