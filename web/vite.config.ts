import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

const backendPort = process.env.BACKEND_PORT || '25297'
const backendURL = `http://localhost:${backendPort}`

// Bind to localhost by default. Set VITE_DEV_HOST=0.0.0.0 (or another
// interface) ONLY when LAN access is needed (e.g., testing on a phone
// against your local laptop). Binding to 0.0.0.0 with allowedHosts:true
// exposes the proxied backend endpoints to anyone on your network.
const devHost = process.env.VITE_DEV_HOST || 'localhost'
const allowAllHosts = process.env.VITE_DEV_ALLOW_ALL_HOSTS === '1'

export default defineConfig({
  plugins: [react()],
  server: {
    host: devHost,
    port: 5173,
    // allowedHosts: true is permissive (accepts any Host header). Keep
    // it gated behind an explicit env var so the default localhost
    // dev experience is also strict against rebinding attacks. Vite
    // expects either `true` or `string[]`; `false` is not a documented
    // value, so pass the dev hostnames explicitly for the strict default.
    // dev.clawvisor.com resolves to 127.0.0.1 via /etc/hosts when running
    // the local HTTPS terminator (scripts/dev.sh + Caddyfile), and the
    // app's auth config requires that origin for WebAuthn/SAML/OAuth.
    allowedHosts: allowAllHosts ? true : ['dev.clawvisor.com'],
    proxy: {
      '/api': backendURL,
      '/skill': backendURL,
      '/health': backendURL,
      '/ready': backendURL,
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
