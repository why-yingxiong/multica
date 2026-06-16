import type { Metadata } from "next";
import { RedirectIfAuthenticated } from "@/features/landing/components/redirect-if-authenticated";
import { RedirectToLogin } from "@/features/landing/components/redirect-to-login";

export const metadata: Metadata = {
  title: {
    absolute: "HeroGameStudio — Project Management for Human + Agent Teams",
  },
  description:
    "Open-source platform that turns coding agents into real teammates. Assign tasks, track progress, compound skills.",
  openGraph: {
    title: "HeroGameStudio — Project Management for Human + Agent Teams",
    description: "Manage your human + agent workforce in one place.",
    url: "/",
  },
  alternates: {
    canonical: "/",
  },
};

// Demo build: the root route is a pure auth router — no marketing landing.
// Authenticated visitors go to their workspace (RedirectIfAuthenticated);
// everyone else is sent to /login (RedirectToLogin).
export default function LandingPage() {
  return (
    <>
      <RedirectIfAuthenticated />
      <RedirectToLogin />
    </>
  );
}
