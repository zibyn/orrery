import { describe, expect, it } from 'vitest'

import { nodeCapabilities, nodeGPUs } from './node-detail'

describe('nodeCapabilities', () => {
  it('reads the Runtime Provider and each capability level', () => {
    expect(
      nodeCapabilities({
        runtime_provider: 'containerd',
        entries: [
          { name: 'exec_supervisor', value: 'enforced' },
          { name: 'gpu_memory_limit', value: 'unsupported' },
        ],
      }),
    ).toEqual({
      provider: 'containerd',
      entries: [
        { name: 'exec_supervisor', value: 'enforced' },
        { name: 'gpu_memory_limit', value: 'unsupported' },
      ],
    })
  })

  it('is empty for a Node that has not reported Capabilities', () => {
    expect(nodeCapabilities(null)).toEqual({ provider: null, entries: [] })
    expect(nodeCapabilities({ entries: null })).toEqual({ provider: null, entries: [] })
  })
})

describe('nodeGPUs', () => {
  it('pairs each GPU with its last reported health', () => {
    expect(
      nodeGPUs(
        [
          { uuid: 'gpu-a', model: 'A100' },
          { uuid: 'gpu-b', model: 'A100' },
        ],
        {
          gpu_health: [
            { uuid: 'gpu-b', state: 'unhealthy' },
            { uuid: 'gpu-a', state: 'healthy' },
          ],
        },
      ),
    ).toEqual([
      { uuid: 'gpu-a', model: 'A100', health: 'healthy' },
      { uuid: 'gpu-b', model: 'A100', health: 'unhealthy' },
    ])
  })

  it('has no health before the first Heartbeat', () => {
    expect(nodeGPUs([{ uuid: 'gpu-a', model: 'A100' }], null)).toEqual([
      { uuid: 'gpu-a', model: 'A100', health: null },
    ])
  })
})
