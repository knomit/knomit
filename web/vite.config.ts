import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// https://vite.dev/config/
export default defineConfig({
  plugins: [react()],
  server: {
    proxy: {
      '/api': {
        target: 'http://localhost:3000',
        // Prevent Accept-Encoding: gzip on proxied requests — compression
        // buffers SSE event streams, breaking real-time updates.
        headers: { 'Accept-Encoding': 'identity' },
      },
      '/mcp': 'http://localhost:3000',
      '/git': 'http://localhost:3000',
      '/docs': 'http://localhost:3000',
    },
  },
})
