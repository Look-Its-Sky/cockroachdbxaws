import { SignInForm } from "@/components/sign-in-form";

export default function SignInPage() {
  return (
    <main className="auth-shell">
      <section className="auth-context">
        <p className="eyebrow">STATIC LOG ANALYSIS</p>
        <h1>Operations, without the noise.</h1>
        <p>A private view of ingestion, investigations, and agent queue delivery.</p>
      </section>
      <SignInForm />
    </main>
  );
}
