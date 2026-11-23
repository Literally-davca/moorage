import { defineConfig } from 'vite';
import { svelte } from '@sveltejs/vite-plugin-svelte';

// The built UI is embedded into the Go binary from internal/web/dist.
export default defineConfig({
  plugins: [svelte()],
  build: {
    outDir: '../internal/web/dist',
    emptyOutDir: true,
    assetsInlineLimit: 0, // keep fonts as files, CSP has no data: fonts
    chunkSizeWarningLimit: 700,
  },
  server: {
    port: 5173,
    proxy: {
      '/api': { target: 'http://localhost:5080', ws: true, changeOrigin: false },
    },
  },
});
