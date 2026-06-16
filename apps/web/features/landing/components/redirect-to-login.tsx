"use client";

import { useEffect } from "react";
import { useRouter } from "next/navigation";
import { useAuthStore } from "@multica/core/auth";

/**
 * Demo build: the root route sends unauthenticated visitors straight to
 * /login instead of the marketing landing page. Mirrors
 * RedirectIfAuthenticated (which handles the logged-in case) — together
 * they make `/` a pure router: authed → workspace, unauthed → /login.
 *
 * Renders nothing. Waits for auth to resolve (isLoading) so it does not
 * redirect during the initial hydration pass before the session is known.
 * Uses router.replace so `/` never enters browser history.
 */
export function RedirectToLogin() {
  const router = useRouter();
  const user = useAuthStore((s) => s.user);
  const isLoading = useAuthStore((s) => s.isLoading);

  useEffect(() => {
    if (isLoading || user) return;
    router.replace("/login");
  }, [isLoading, user, router]);

  return null;
}
