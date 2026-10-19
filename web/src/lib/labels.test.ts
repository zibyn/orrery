import { afterEach, describe, expect, it } from 'vitest'

import { ApiError } from '@/api'
import { overwriteGetLocale } from '@/paraglide/runtime'

import { causeLabel, problemOf, stateLabel } from './labels'

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
