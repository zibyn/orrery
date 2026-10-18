import { describe, expect, it } from 'vitest'

import type { CapacityNode, Node } from '@/api'

import { mergeNodeCapacity, nodeListPollInterval } from './nodes'

function node(overrides: Partial<Node>): Node {
  return {
    id: 'node-1',
    name: 'node-1',
    status: 'ready',
    paused: false,
    lost_at: null,
    agent_version: null,
    proto_major: 1,
    proto_minor: 0,
    last_seen_at: null,
    runtime_health: null,
    capabilities: null,
    hardware: null,
    gpus: [],
    created_at: '2026-10-18T00:00:00Z',
    ...overrides,
  }
}

function capacity(overrides: Partial<CapacityNode>): CapacityNode {
  return {
    node_id: 'node-1',
    name: 'node-1',
    status: 'ready',
    gpus: [],
    ...overrides,
  }
}

describe('mergeNodeCapacity', () => {
  it('attaches capacity totals and allocations to the matching Node', () => {
    const rows = mergeNodeCapacity(
      [node({ gpus: [{ uuid: 'gpu-a', model: 'A100' }] })],
      [capacity({ gpus: [{ model: 'A100', total: 8, allocated: 3 }] })],
    )
    expect(rows).toEqual([
      {
        id: 'node-1',
        name: 'node-1',
        status: 'ready',
        paused: false,
        gpus: [{ model: 'A100', total: 8, allocated: 3 }],
      },
    ])
  })

  it('falls back to the Node GPU inventory when capacity has no entry', () => {
    const rows = mergeNodeCapacity(
      [node({ gpus: [{ uuid: 'a', model: 'A100' }, { uuid: 'b', model: 'A100' }] })],
      [],
    )
    expect(rows[0].gpus).toEqual([{ model: 'A100', total: 2, allocated: 0 }])
  })

  it('keeps capacity-only Nodes visible but without pause state', () => {
    const rows = mergeNodeCapacity([], [capacity({ node_id: 'node-2', name: 'node-2' })])
    expect(rows).toEqual([
      { id: 'node-2', name: 'node-2', status: 'ready', paused: null, gpus: [] },
    ])
  })
})

describe('nodeListPollInterval', () => {
  it('polls while any Node is not lost', () => {
    expect(nodeListPollInterval([{ status: 'ready' }, { status: 'lost' }])).toBe(5000)
    expect(nodeListPollInterval([{ status: 'lost' }, { status: 'unavailable' }])).toBe(5000)
  })

  it('treats ready, paused, unavailable and outdated as non-terminal', () => {
    for (const status of ['ready', 'paused', 'unavailable', 'outdated'] as const) {
      expect(nodeListPollInterval([{ status }])).toBe(5000)
    }
  })

  it('stops when every Node is lost', () => {
    expect(nodeListPollInterval([{ status: 'lost' }])).toBe(false)
    expect(nodeListPollInterval([])).toBe(false)
    expect(nodeListPollInterval(undefined)).toBe(false)
  })
})
