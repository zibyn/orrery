import { toast } from 'sonner'

import { ApiError } from '@/api'
import { problemOf } from '@/lib/labels'

// Every failed request surfaces as a toast, never inline on the page. The id
// makes a repeated problem (a polling query) replace its toast, not stack.
// A 401 means the Session ended, so the browser goes back through login and
// returns here (ADR-0020).
export function notifyError(error: unknown) {
  if (error instanceof ApiError && error.status === 401) {
    const here = window.location.pathname + window.location.search
    window.location.assign(`/login?return=${encodeURIComponent(here)}`)
    return
  }
  const { title, detail } = problemOf(error)
  toast.error(title, { id: `${title}\n${detail ?? ''}`, description: detail })
}
