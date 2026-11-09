import { afterEach, describe, expect, it } from 'vitest'

import { ApiError } from '@/api'
import { overwriteGetLocale } from '@/paraglide/runtime'

import { causeLabel, identityLabel, idleStopLabel, problemOf, stateLabel } from './labels'

afterEach(() => {
  overwriteGetLocale(() => 'en')
})

describe('stateLabel', () => {
  it('localizes a known state in both languages', () => {
    overwriteGetLocale(() => 'en')
    expect(stateLabel('ready')).toBe('ready')
    overwriteGetLocale(() => 'zh')
    expect(stateLabel('ready')).toBe('就绪')
  })

  it('falls back to the server value for an unknown state', () => {
    overwriteGetLocale(() => 'zh')
    expect(stateLabel('brand_new_state')).toBe('brand_new_state')
  })
})

describe('causeLabel', () => {
  it('localizes the closed cause catalog', () => {
    overwriteGetLocale(() => 'zh')
    expect(causeLabel('timed_out')).toBe('超时')
    expect(causeLabel('build_interrupted')).toBe('构建中断')
  })
})

describe('problemOf', () => {
  const error = new ApiError(409, 'conflict', 'the Sandbox is not ready')

  it('keeps the server message in English', () => {
    overwriteGetLocale(() => 'en')
    expect(problemOf(error)).toEqual({ title: 'the Sandbox is not ready' })
  })

  it('shows a localized code title and keeps the detail in Chinese', () => {
    overwriteGetLocale(() => 'zh')
    expect(problemOf(error)).toEqual({
      title: '状态冲突',
      detail: 'the Sandbox is not ready',
    })
  })

  it('falls back to the code when it is not in the catalog', () => {
    overwriteGetLocale(() => 'zh')
    expect(problemOf(new ApiError(418, 'teapot', ''))).toEqual({ title: 'teapot' })
  })
})

describe('idleStopLabel', () => {
  it('reads 0 as off, whole minutes as minutes, the rest as seconds', () => {
    overwriteGetLocale(() => 'en')
    expect(idleStopLabel(0)).toBe('off')
    expect(idleStopLabel(900)).toBe('after 15 min idle')
    expect(idleStopLabel(90)).toBe('after 90 s idle')
  })
})

describe('identityLabel', () => {
  it('tells a Polaris-linked User from a machine User', () => {
    overwriteGetLocale(() => 'en')
    expect(identityLabel({ machine: false })).toBe('Polaris')
    expect(identityLabel({ machine: true })).toBe('Machine')
    overwriteGetLocale(() => 'zh')
    expect(identityLabel({ machine: true })).toBe('机器用户')
  })
})
