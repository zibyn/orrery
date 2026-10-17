/// <reference types="vitest/config" />
import { fileURLToPath, URL } from 'node:url'

import tailwindcss from '@tailwindcss/vite'
import { tanstackRouter } from '@tanstack/router-plugin/vite'
import react from '@vitejs/plugin-react'
import { defineConfig } from 'vite'

// The built files land in web/dist and are embedded with -tags embed
// (api.md §8). The modulepreload polyfill is an inline script, which the
// console's Content-Security-Policy forbids.
export default defineConfig({
  plugins: [
    tanstackRouter({ target: 'react', autoCodeSplitting: true }),
    react(),
    tailwindcss(),
  ],
  resolve: {
    alias: { '@': fileURLToPath(new URL('./src', import.meta.url)) },
  },
  build: { modulePreload: { polyfill: false } },
  server: { proxy: { '/api': 'http://localhost:8080' } },
  test: { environment: 'node' },
})
