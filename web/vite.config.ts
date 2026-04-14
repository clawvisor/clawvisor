import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    allowedHosts: true,
    proxy: {
      '/api': 'http://localhost:25297',
      '/skill': 'http://localhost:25297',
      '/health': 'http://localhost:25297',
      '/ready': 'http://localhost:25297',
      '/ws': {
        target: 'http://localhost:25297',
        ws: true,
      },
    },
  },
  build: {
    outDir: 'dist',
    sourcemap: false,
  },
})
