import { resolve } from "node:path";
import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// Multi-page build: three entries matching the FastAPI routes that serve
// them (/ -> index.html, /details -> details.html, /login -> login.html).
// Output goes straight into ../static, which FastAPI serves at /static.
export default defineConfig({
  base: "/static/",
  plugins: [react()],
  build: {
    outDir: "../static",
    emptyOutDir: true,
    rollupOptions: {
      input: {
        index: resolve(__dirname, "index.html"),
        details: resolve(__dirname, "details.html"),
        login: resolve(__dirname, "login.html"),
      },
    },
  },
  server: {
    // Dev proxy: `npm run dev` forwards API/auth calls to the Python service.
    proxy: {
      "/api": "http://localhost:8080",
      "/login": "http://localhost:8080",
      "/logout": "http://localhost:8080",
    },
  },
});
