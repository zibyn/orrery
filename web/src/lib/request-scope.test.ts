import { describe, expect, it } from 'vitest'

import { isAllowedConsoleUrl } from './request-scope'

const origin = 'http://127.0.0.1:18080'
const scope = { origin, appsDomain: 'apps.test' }

describe('isAllowedConsoleUrl', () => {
  it('allows console pages, API calls and assets on the console origin', () => {
    expect(isAllowedConsoleUrl(`${origin}/`, scope)).toBe(true)
    expect(isAllowedConsoleUrl(`${origin}/sandboxes?x=1`, scope)).toBe(true)
    expect(isAllowedConsoleUrl(`${origin}/sandboxes/abc/execs?exec=1`, scope)).toBe(true)
    expect(isAllowedConsoleUrl(`${origin}/login?return=%2Fsandboxes`, scope)).toBe(true)
    expect(isAllowedConsoleUrl(`${origin}/admin/users`, scope)).toBe(true)
    expect(isAllowedConsoleUrl(`${origin}/api/v1/sandboxes`, scope)).toBe(true)
    expect(isAllowedConsoleUrl(`${origin}/assets/index-abc.js`, scope)).toBe(true)
    expect(isAllowedConsoleUrl('/assets/index-abc.js', scope)).toBe(true)
  })

  it('rejects private and non-public paths on the console origin', () => {
    expect(isAllowedConsoleUrl(`${origin}/metrics`, scope)).toBe(false)
    expect(isAllowedConsoleUrl(`${origin}/api/admin/users`, scope)).toBe(false)
    expect(isAllowedConsoleUrl(`${origin}/api/v2/things`, scope)).toBe(false)
    expect(isAllowedConsoleUrl(`${origin}/api`, scope)).toBe(false)
    expect(isAllowedConsoleUrl(`${origin}/internal/status`, scope)).toBe(false)
  })

  it('allows same-origin WebSockets', () => {
    expect(isAllowedConsoleUrl('ws://127.0.0.1:18080/api/v1/execs/1/output?follow=true', scope)).toBe(true)
  })

  it('allows a port-forward subdomain regardless of its path', () => {
    expect(
      isAllowedConsoleUrl('https://8080--3f2a1c1e-0000-4000-8000-000000000000.apps.test/', scope),
    ).toBe(true)
    expect(
      isAllowedConsoleUrl(
        'https://8080--3f2a1c1e-0000-4000-8000-000000000000.apps.test/private/path?x=1',
        scope,
      ),
    ).toBe(true)
  })

  it('rejects another origin', () => {
    expect(isAllowedConsoleUrl('https://evil.example/api/v1/users', scope)).toBe(false)
    expect(isAllowedConsoleUrl('//evil.example/api/v1/users', scope)).toBe(false)
    expect(isAllowedConsoleUrl(`${origin.replace('18080', '18081')}/api/v1/users`, scope)).toBe(false)
  })

  it('rejects a lookalike host that only ends with the apps domain', () => {
    expect(isAllowedConsoleUrl('https://8080--abc.evil.example/apps.test', scope)).toBe(false)
    expect(isAllowedConsoleUrl('https://8080--abc.apps.test.evil.example/', scope)).toBe(false)
  })

  it('rejects port-forward hosts when no apps domain is configured', () => {
    expect(
      isAllowedConsoleUrl('https://8080--3f2a1c1e-0000-4000-8000-000000000000.apps.test/', {
        origin,
      }),
    ).toBe(false)
  })

  it('rejects non-HTTP schemes and malformed URLs', () => {
    expect(isAllowedConsoleUrl('data:text/plain,hello', scope)).toBe(false)
    expect(isAllowedConsoleUrl('file:///etc/passwd', scope)).toBe(false)
    expect(isAllowedConsoleUrl('http://[::1', scope)).toBe(false)
  })
})
