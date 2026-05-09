import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

const backendPort = process.env.BACKEND_PORT || '25297'
const backendURL = `http://localhost:${backendPort}`

export default defineConfig({
  plugins: [react()],
  server: {
    host: '0.0.0.0',
    port: 5173,
    allowedHosts: true,
    proxy: {
      '/api': backendURL,
      '/skill': backendURL,
      '/health': backendURL,
      '/ready': backendURL,
      // Lite-proxy LLM endpoint (Anthropic + OpenAI compatible) and the
      // resolver. Agents pointing ANTHROPIC_BASE_URL / OPENAI_BASE_URL
      // at the dev server need these proxied through.
      '/v1': backendURL,
      '/proxy': backendURL,
      '/ws': {
        target: backendURL,
        ws: true,
      },
    },
  },
  build: {
    outDir: 'dist',
    sourcemap: false,
  },
})
