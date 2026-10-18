import { describe, expect, it } from 'vitest'

import { matchesCurrentSession } from './current-session'

const session = { id: 'sess-1', kind: 'session' as const }
const token = { id: 'sess-1', kind: 'token' as const }

describe('matchesCurrentSession', () => {
  it('marks the Session row whose id was stored at login', () => {
    expect(matchesCurrentSession(session, 'sess-1')).toBe(true)
  })

  it('does not mark another Session', () => {
    expect(matchesCurrentSession({ id: 'sess-2', kind: 'session' }, 'sess-1')).toBe(false)
  })

  it('never marks an API token, even if the id matches', () => {
    expect(matchesCurrentSession(token, 'sess-1')).toBe(false)
  })

  it('renders no marker when no id is stored (for example after OIDC login)', () => {
    expect(matchesCurrentSession(session, null)).toBe(false)
    expect(matchesCurrentSession(session, '')).toBe(false)
  })
})
