"use client";

import { useRouter } from "next/navigation";
import { useCallback, useEffect, useTransition } from "react";

export function RefreshControl() {
  const router = useRouter();
  const [pending, startTransition] = useTransition();
  const refresh = useCallback(() => startTransition(() => router.refresh()), [router]);

  useEffect(() => {
    const timer = window.setInterval(refresh, 10_000);
    return () => window.clearInterval(timer);
  }, [refresh]);

  return (
    <button className="refresh" onClick={refresh} disabled={pending} aria-label="Refresh dashboard data">
      <span className={pending ? "pulse" : "live-dot"} />
      {pending ? "Refreshing" : "Live · 10s"}
    </button>
  );
}
