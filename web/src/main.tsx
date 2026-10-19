import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { createRouter, RouterProvider } from '@tanstack/react-router'
import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'

import { LocaleProvider } from './lib/i18n'
import { getLocale } from './paraglide/runtime'
import { routeTree } from './routeTree.gen'
import './index.css'

const queryClient = new QueryClient()
const router = createRouter({ routeTree, context: { queryClient } })

// Set the document language before the first render so assistive technology
// sees the resolved locale immediately (api.md §8).
document.documentElement.lang = getLocale()

declare module '@tanstack/react-router' {
  interface Register {
    router: typeof router
  }
}

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <LocaleProvider>
      <QueryClientProvider client={queryClient}>
        <RouterProvider router={router} />
      </QueryClientProvider>
    </LocaleProvider>
  </StrictMode>,
)
