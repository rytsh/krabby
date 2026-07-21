import { defineConfig } from "vite";
import { svelte } from "@sveltejs/vite-plugin-svelte";
import tailwindcss from "@tailwindcss/vite";

// Build a static SPA into dist/, which the Go server embeds and serves.
// During dev, proxy API/MCP calls to a locally running krabby on :8080.
export default defineConfig({
  plugins: [tailwindcss(), svelte()],
  build: {
    // Output into the Go-visible internal/server package so it can be embedded.
    // _ui itself is ignored by the go tool (leading underscore), so the dist
    // must live outside it.
    outDir: "../internal/server/dist",
    emptyOutDir: true,
  },
  server: {
    proxy: {
      "/api": "http://localhost:8080",
      "/mcp": "http://localhost:8080",
      "/healthz": "http://localhost:8080",
    },
  },
});
