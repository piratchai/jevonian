import { readFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

import tailwindcss from "@tailwindcss/vite";
import react from "@vitejs/plugin-react";
import { defineConfig } from "vite-plus";

const root = dirname(fileURLToPath(import.meta.url));
const { version } = JSON.parse(readFileSync(resolve(root, "../package.json"), "utf8")) as {
  version: string;
};

export default defineConfig({
  root,
  plugins: [react(), tailwindcss()],
  resolve: {
    alias: { "@": resolve(root, "src") },
  },
  define: {
    __JEVONIAN_VERSION__: JSON.stringify(version),
  },
  build: {
    outDir: resolve(root, "../dist/web"),
    emptyOutDir: true,
  },
  server: {
    host: true,
    // Dev ports sit far from the production defaults (8787 api / 5173 web) so `npm run dev`
    // never collides with a running jevonian instance or another vite dev server.
    port: Number(process.env.JEVONIAN_WEB_PORT ?? 15174),
    proxy: {
      "/api": `http://127.0.0.1:${Number(process.env.JEVONIAN_PORT ?? 18888)}`,
      "/v1": `http://127.0.0.1:${Number(process.env.JEVONIAN_PORT ?? 18888)}`,
      "/healthz": `http://127.0.0.1:${Number(process.env.JEVONIAN_PORT ?? 18888)}`,
    },
  },
});
