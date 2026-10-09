import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// In development the Go server runs separately (`divvy serve`); proxy /api to
// it so the app stays same-origin and never embeds the token in code.
// Override with VITE_DEV_TARGET=http://127.0.0.1:<port>.
const devTarget = process.env.VITE_DEV_TARGET ?? 'http://127.0.0.1:8787'

export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    proxy: {
      '/api': {
        target: devTarget,
        changeOrigin: true,
      },
    },
  },
  build: {
    outDir: 'dist',
    sourcemap: false,
  },
})
