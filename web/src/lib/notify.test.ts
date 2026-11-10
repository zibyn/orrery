import { afterEach, describe, expect, it, vi } from 'vitest'

import { ApiError } from '@/api'

const toastError = vi.hoisted(() => vi.fn())
vi.mock('sonner', () => ({ toast: { error: toastError } }))

import { notifyError } from './notify'

afterEach(() => {
  vi.unstubAllGlobals()
  toastError.mockReset()
})

describe('notifyError', () => {
  it('sends an ended Session back through login to the current page', () => {
    const assign = vi.fn()
    vi.stubGlobal('window', { location: { pathname: '/sandboxes', search: '?owner=me', assign } })
    notifyError(new ApiError(401, 'unauthenticated', 'expired'))
    expect(assign).toHaveBeenCalledWith('/login?return=%2Fsandboxes%3Fowner%3Dme')
    expect(toastError).not.toHaveBeenCalled()
  })

  it('toasts any other problem under one id, so a repeat replaces it', () => {
    notifyError(new ApiError(403, 'permission_denied', 'nope'))
    notifyError(new ApiError(403, 'permission_denied', 'nope'))
    expect(toastError).toHaveBeenCalledTimes(2)
    expect(toastError.mock.calls[0][1].id).toBe(toastError.mock.calls[1][1].id)
  })
})
