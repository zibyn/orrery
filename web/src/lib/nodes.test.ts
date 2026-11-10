import { describe, expect, it } from 'vitest'

import type { CapacityNode, Node } from '@/api'

import { diskBelowWatermark, formatSince, mergeNodeCapacity, nodeListPollInterval } from './nodes'

function node(overrides: Partial<Node>): Node {
  return {
    id: 'node-1',
    name: 'node-1',
    status: 'ready',
    status_reason: null,
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
    cpu: null,
    memory: null,
    disk: null,
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
        statusReason: null,
        lastSeenAt: null,
        unhealthyGPUs: 0,
        gpus: [{ model: 'A100', total: 8, allocated: 3 }],
        cpu: null,
        memory: null,
        disk: null,
      },
    ])
  })

  it('carries why a Node is unavailable, its last heartbeat and its unhealthy cards', () => {
    const [row] = mergeNodeCapacity(
      [
        node({
          status: 'unavailable',
          status_reason: 'heartbeat_stale',
          last_seen_at: '2026-11-10T08:00:00Z',
          hardware: {
            gpu_health: [
              { uuid: 'a', state: 'healthy' },
              { uuid: 'b', state: 'unhealthy' },
              { uuid: 'c', state: 'unhealthy' },
            ],
          },
        }),
      ],
      [],
    )
    expect(row.statusReason).toBe('heartbeat_stale')
    expect(row.lastSeenAt).toBe('2026-11-10T08:00:00Z')
    expect(row.unhealthyGPUs).toBe(2)
  })

  it('counts no unhealthy cards when the Node has not reported hardware', () => {
    expect(mergeNodeCapacity([node({ hardware: null })], [])[0].unhealthyGPUs).toBe(0)
    expect(mergeNodeCapacity([node({ hardware: { disk_free: 1 } })], [])[0].unhealthyGPUs).toBe(0)
  })

  it('attaches CPU and memory pools and disk from capacity', () => {
    const cpu = { gpu_pool: { total: 32, allocated: 8 }, general_pool: { total: 60, allocated: 4 } }
    const memory = { gpu_pool: null, general_pool: { total: 1 << 30, allocated: 1 << 29 } }
    const disk = { total: 1000, free: 50, watermark: 0.1 }
    const [row] = mergeNodeCapacity([node({})], [capacity({ cpu, memory, disk })])
    expect(row.cpu).toEqual(cpu)
    expect(row.memory).toEqual(memory)
    expect(row.disk).toEqual(disk)
  })

  it('falls back to the Node GPU inventory when capacity has no entry', () => {
    const rows = mergeNodeCapacity(
      [node({ gpus: [{ uuid: 'a', model: 'A100' }, { uuid: 'b', model: 'A100' }] })],
      [],
    )
    expect(rows[0].gpus).toEqual([{ model: 'A100', total: 2, allocated: 0 }])
    expect(rows[0]).toMatchObject({ cpu: null, memory: null, disk: null })
  })

  it('keeps capacity-only Nodes visible but without pause state', () => {
    const rows = mergeNodeCapacity([], [capacity({ node_id: 'node-2', name: 'node-2' })])
    expect(rows).toEqual([
      {
        id: 'node-2',
        name: 'node-2',
        status: 'ready',
        paused: null,
        statusReason: null,
        lastSeenAt: null,
        unhealthyGPUs: 0,
        gpus: [],
        cpu: null,
        memory: null,
        disk: null,
      },
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

describe('formatSince', () => {
  const now = Date.parse('2026-11-10T08:00:00Z')

  it('names the largest whole unit since the instant', () => {
    expect(formatSince('2026-11-10T07:59:57Z', now, 'en')).toBe('3 seconds ago')
    expect(formatSince('2026-11-10T07:58:00Z', now, 'en')).toBe('2 minutes ago')
    expect(formatSince('2026-11-10T05:00:00Z', now, 'en')).toBe('3 hours ago')
    expect(formatSince('2026-11-08T08:00:00Z', now, 'en')).toBe('2 days ago')
  })

  it('shows a dash for a Node that never sent a Heartbeat', () => {
    expect(formatSince(null, now, 'en')).toBe('—')
  })
})

describe('diskBelowWatermark', () => {
  it('is true once free disk drops under total × watermark', () => {
    expect(diskBelowWatermark({ total: 1000, free: 99, watermark: 0.1 })).toBe(true)
    expect(diskBelowWatermark({ total: 1000, free: 100, watermark: 0.1 })).toBe(false)
    expect(diskBelowWatermark(null)).toBe(false)
  })
})
