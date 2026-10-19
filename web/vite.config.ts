/// <reference types="vitest/config" />
import { paraglideVitePlugin } from '@inlang/paraglide-js'
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
    // The console has no locale URL segment; a stored choice wins, then the
    // browser's language, then English (api.md §8).
    paraglideVitePlugin({
      project: './project.inlang',
      outdir: './src/paraglide',
      strategy: ['localStorage', 'preferredLanguage', 'baseLocale'],
      emitTsDeclarations: true,
    }),
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
