import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'
import { fileURLToPath, URL } from 'node:url'

// Served by the Go binary at /designer/ (go:embed of dist/). Fixed asset
// names (no content hashes) follow the EasyScan convention; the embed
// re-globs on every `go build`, so hashes buy nothing here.
export default defineConfig({
  plugins: [vue()],
  base: '/designer/',
  resolve: { alias: { '@': fileURLToPath(new URL('./src', import.meta.url)) } },
  build: {
    outDir: 'dist',
    emptyOutDir: true,
    sourcemap: true,
    target: 'es2022',
    rollupOptions: {
      output: {
        entryFileNames: 'assets/[name].js',
        chunkFileNames: 'assets/[name].js',
        assetFileNames: 'assets/[name][extname]',
      },
    },
  },
  server: {
    port: 5173,
    proxy: {
      '/v1': 'http://localhost:8080',
      '/render': 'http://localhost:8080',
      '/templates': 'http://localhost:8080',
      '/healthz': 'http://localhost:8080',
    },
  },
})
