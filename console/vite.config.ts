import { defineConfig, loadEnv } from "vite";
import react from "@vitejs/plugin-react";

export default defineConfig(({ mode }) => {
  const env = loadEnv(mode, process.cwd(), "");
  const user = env.VITE_DEV_IDENTITY_USER ?? "";
  const team = env.VITE_DEV_IDENTITY_TEAM ?? "";
  const roles = env.VITE_DEV_IDENTITY_ROLES ?? "operator";
  const proxy: Record<string, import("vite").ProxyOptions> = {
    [env.VITE_API_PREFIX ?? env.VITE_API_BASE_PATH ?? "/v1"]: {
      target: env.VITE_API_PROXY_TARGET ?? "http://127.0.0.1:8080",
      changeOrigin: true,
      configure(p) {
        p.on("proxyReq", (proxyReq) => {
          if (user) {
            proxyReq.setHeader("X-OpenSandbox-User", user);
            if (team) {
              proxyReq.setHeader("X-OpenSandbox-Team", team);
            }
            proxyReq.setHeader("X-OpenSandbox-Roles", roles);
          }
        });
      },
    },
  };
  return {
    base: env.VITE_BASE ?? "/console/",
    plugins: [react()],
    server: { proxy },
  };
});
