import { describe, expect, it } from 'vitest'

import { type Sandbox, type SandboxState } from '@/api'
import { isSandboxTerminal, sandboxListPollInterval, sandboxPollInterval } from './sandbox'

function sandbox(state: SandboxState): Sandbox {
  return {
    id: 'id',
    name: 'name',
    labels: {},
    runtime: 'container',
    state,
    cause: null,
    node_id: null,
    snapshot: 'ref',
    cpu: 1,
    memory: 1024,
    idle_stop: 900,
    conditions: [],
    created_at: '2026-01-01T00:00:00Z',
  }
}

describe('isSandboxTerminal', () => {
  it('treats failed, deleted and stopped as terminal', () => {
    expect(isSandboxTerminal('failed')).toBe(true)
    expect(isSandboxTerminal('deleted')).toBe(true)
    // A stopped Sandbox has no autonomous transition; only a user start moves it.
    expect(isSandboxTerminal('stopped')).toBe(true)
  })

  it('keeps polling through every other state, including ready and stopping', () => {
    for (const state of ['pending', 'provisioning', 'ready', 'stopping', 'deleting'] as const) {
      expect(isSandboxTerminal(state)).toBe(false)
    }
    expect(isSandboxTerminal(undefined)).toBe(false)
  })
})

describe('sandboxPollInterval', () => {
  it('polls a detail every 2 seconds until the Sandbox is terminal', () => {
    expect(sandboxPollInterval(undefined)).toBe(2000)
    expect(sandboxPollInterval('provisioning')).toBe(2000)
    expect(sandboxPollInterval('ready')).toBe(2000)
    expect(sandboxPollInterval('stopping')).toBe(2000)
    expect(sandboxPollInterval('stopped')).toBe(false)
    expect(sandboxPollInterval('failed')).toBe(false)
    expect(sandboxPollInterval('deleted')).toBe(false)
  })
})

describe('sandboxListPollInterval', () => {
  it('polls every 5 seconds while any Sandbox is non-terminal', () => {
    expect(sandboxListPollInterval([sandbox('ready'), sandbox('failed')])).toBe(5000)
  })

  it('stops when every Sandbox is terminal', () => {
    expect(sandboxListPollInterval([sandbox('failed'), sandbox('deleted'), sandbox('stopped')])).toBe(
      false,
    )
    expect(sandboxListPollInterval([])).toBe(false)
    expect(sandboxListPollInterval(undefined)).toBe(false)
  })
})
