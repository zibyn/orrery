import { describe, expect, it } from 'vitest'

import type { ExecOutputFrame } from '@/api'
import {
  decodeFrame,
  execListPollInterval,
  formatExecResult,
  frameEnd,
  frameText,
  isResultFrame,
  parseArgv,
  TRUNCATION_NOTICE,
} from './exec-stream'

const base64 = (text: string) => btoa(String.fromCharCode(...new TextEncoder().encode(text)))

describe('decodeFrame', () => {
  it('returns text frames as they are', () => {
    const frame: ExecOutputFrame = { seq: 0, stream: 'stdout', data: 'hello\n' }
    expect(decodeFrame(frame)).toBe('hello\n')
  })

  it('decodes base64 frames into their UTF-8 text', () => {
    const frame: ExecOutputFrame = {
      seq: 3,
      stream: 'stderr',
      data: base64('你好\n'),
      encoding: 'base64',
    }
    expect(decodeFrame(frame)).toBe('你好\n')
  })
})

describe('frameEnd', () => {
  it('advances by the 5-byte frame header plus the UTF-8 payload', () => {
    const frame: ExecOutputFrame = { seq: 100, stream: 'stdout', data: 'abcd' }
    expect(frameEnd(frame)).toBe(109)
  })

  it('uses the decoded byte length for base64 frames', () => {
    // Two invalid-UTF-8 bytes become two replacement characters when decoded,
    // but the record offset still counts the two original bytes.
    const frame: ExecOutputFrame = {
      seq: 0,
      stream: 'stdout',
      data: btoa(String.fromCharCode(0xff, 0xfe)),
      encoding: 'base64',
    }
    expect(frameEnd(frame)).toBe(7)
  })

  it('counts multibyte UTF-8 text as bytes, not code points', () => {
    const frame: ExecOutputFrame = { seq: 5, stream: 'stdout', data: 'é' }
    expect(frameEnd(frame)).toBe(5 + 5 + 2)
  })
})

describe('frameText', () => {
  it('marks a truncated frame visibly instead of writing it as output', () => {
    const frame: ExecOutputFrame = { seq: 0, stream: 'truncated', data: '' }
    const text = frameText(frame)
    expect(text).toContain(TRUNCATION_NOTICE.trim())
    expect(text).toContain('CLI')
  })

  it('writes stdout and stderr frames unchanged', () => {
    expect(frameText({ seq: 0, stream: 'stdout', data: 'out' })).toBe('out')
    expect(frameText({ seq: 2, stream: 'stderr', data: 'err' })).toBe('err')
  })
})

describe('isResultFrame', () => {
  it('distinguishes the final result frame from data frames', () => {
    expect(isResultFrame({ type: 'result', cause: 'exited', log_truncated: false })).toBe(true)
    expect(isResultFrame({ seq: 0, stream: 'stdout', data: '' })).toBe(false)
    expect(isResultFrame(null)).toBe(false)
    expect(isResultFrame('result')).toBe(false)
  })
})

describe('formatExecResult', () => {
  it('shows the cause, exit code and truncation', () => {
    expect(formatExecResult({ cause: 'exited', exit_code: 0, log_truncated: false })).toBe(
      'exited (exit code 0)',
    )
    expect(formatExecResult({ cause: 'exited', exit_code: 2, log_truncated: true })).toBe(
      'exited (exit code 2) · log truncated',
    )
  })

  it('shows a signal when there is no exit code', () => {
    expect(formatExecResult({ cause: 'canceled', signal: 9 })).toBe('canceled (signal 9)')
    expect(formatExecResult({ cause: 'runtime_lost' })).toBe('runtime_lost')
  })
})

describe('execListPollInterval', () => {
  it('polls while an Exec is not finished and stops once every one is', () => {
    expect(
      execListPollInterval([
        { state: 'finished' },
        { state: 'running' },
      ]),
    ).toBe(5000)
    expect(execListPollInterval([{ state: 'finished' }])).toBe(false)
    expect(execListPollInterval([])).toBe(false)
  })
})

describe('parseArgv', () => {
  it('splits on whitespace and drops empty entries', () => {
    expect(parseArgv('  /bin/sh   -l ')).toEqual(['/bin/sh', '-l'])
    expect(parseArgv('')).toEqual([])
    expect(parseArgv('   ')).toEqual([])
  })
})
