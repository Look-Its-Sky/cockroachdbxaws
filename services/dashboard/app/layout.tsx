import type { Metadata } from "next";
import "./globals.css";

export const metadata: Metadata = {
  title: "Static Log Analysis · Operations",
  description: "A safe operational overview of the static log analysis pipeline.",
  robots: { index: false, follow: false },
};

export default function RootLayout({ children }: Readonly<{ children: React.ReactNode }>) {
  return (
    <html lang="en">
      <body>{children}</body>
    </html>
  );
}
