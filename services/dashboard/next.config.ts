import type { NextConfig } from "next";

const nextConfig: NextConfig = {
  output: "standalone",
  poweredByHeader: false,
  agentRules: false,
  experimental: {
    useTypeScriptCli: false,
  },
};

export default nextConfig;
