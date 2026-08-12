import { SignOut } from "@/components/sign-out";

export default function UnauthorizedPage() {
  return (
    <main className="centered-message">
      <p className="eyebrow">ACCESS RESTRICTED</p>
      <h1>This account is not approved.</h1>
      <p>Your account is authenticated but does not have the required dashboard administrator role.</p>
      <SignOut />
    </main>
  );
}
