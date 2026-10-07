import { resolve } from "node:path"
import tailwindcss from "@tailwindcss/vite"
import react from "@vitejs/plugin-react"
import { defineConfig } from "vite"

// The panel is served by the Go binary from internal/web/static (embedded),
// at /next/ while it replaces the legacy dashboard screen by screen. Its CSP
// is default-src 'self': no inline scripts or <style> elements, so nothing
// here may inject either (CSSOM element.style is fine).
export default defineConfig({
  base: "/next/",
  plugins: [react(), tailwindcss()],
  resolve: {
    alias: {
      "@": resolve(import.meta.dirname, "./src"),
      // See src/lib/stubs/radix-dialog.tsx.
      "@radix-ui/react-dialog": resolve(import.meta.dirname, "./src/lib/stubs/radix-dialog.tsx"),
    },
  },
  build: {
    outDir: resolve(import.meta.dirname, "../internal/web/static/next"),
    emptyOutDir: true,
    // One small file per lazy screen is fine; keep names stable-ish for diffs.
    chunkSizeWarningLimit: 900,
  },
  server: {
    // npm run dev: the API is a running `wpgenie serve` (or the preview harness).
    proxy: { "/api": process.env.WPGENIE_API ?? "http://127.0.0.1:8080" },
  },
})
