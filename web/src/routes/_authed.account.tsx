import { useForm } from '@tanstack/react-form'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { createFileRoute } from '@tanstack/react-router'
import { useState } from 'react'
import { z } from 'zod'

import { api, type CreatedToken, type ListResponse, type Token, type User } from '@/api'
import { TokenReveal } from '@/components/token-reveal'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { matchesCurrentSession, readCurrentSessionId, forgetCurrentSession } from '@/lib/current-session'
import { expiresAtFromInput, formatTimestamp, scopeOptions } from '@/lib/tokens'
import { messageOf } from '@/lib/utils'

const tokenSchema = z.object({
  name: z.string().trim().min(1, 'Enter a name'),
  scope: z.enum(['read_only', 'user', 'admin']),
  expires_at: z.string(),
})

const passwordSchema = z.object({
  old: z.string().min(1, 'Enter your current password'),
  new: z.string().min(12, 'The new password must be at least 12 characters'),
})

type NewToken = {
  name: string
  scope: (typeof scopeOptions)[number]
  expires_at?: string
}

export const Route = createFileRoute('/_authed/account')({
  component: AccountPage,
})

function AccountPage() {
  const { user } = Route.useRouteContext()
  const queryClient = useQueryClient()
  // Reads the id stored by the password login. After an OIDC login no id is
  // stored, so no row is marked (api.md §8).
  const [currentId] = useState(readCurrentSessionId)
  const [created, setCreated] = useState<CreatedToken | null>(null)
  const [tokenError, setTokenError] = useState('')
  const [passwordError, setPasswordError] = useState('')

  const tokens = useQuery({
    queryKey: ['tokens'],
    queryFn: () => api<ListResponse<Token>>('GET', '/users/me/tokens'),
  })

  const createToken = useMutation({
    mutationFn: (body: NewToken) => api<CreatedToken>('POST', '/users/me/tokens', body),
    onSuccess: () => void queryClient.invalidateQueries({ queryKey: ['tokens'] }),
  })

  const revokeToken = useMutation({
    mutationFn: (id: string) => api<Token>('DELETE', `/users/me/tokens/${id}`),
    onSuccess: () => {
      setTokenError('')
      void queryClient.invalidateQueries({ queryKey: ['tokens'] })
    },
    onError: (err) => setTokenError(messageOf(err)),
  })

  const changePassword = useMutation({
    mutationFn: (body: { old: string; new: string }) =>
      api<User>('PUT', '/users/me/password', body),
  })

  const createForm = useForm({
    defaultValues: { name: '', scope: 'user' as NewToken['scope'], expires_at: '' },
    onSubmit: async ({ value }) => {
      setTokenError('')
      const parsed = tokenSchema.safeParse(value)
      if (!parsed.success) {
        setTokenError(parsed.error.issues[0]?.message ?? 'Invalid input')
        return
      }
      const expiresAt = expiresAtFromInput(parsed.data.expires_at)
      if (parsed.data.expires_at && !expiresAt) {
        setTokenError('Enter a valid expiry')
        return
      }
      if (expiresAt && new Date(expiresAt) <= new Date()) {
        setTokenError('Expiry must be in the future')
        return
      }
      try {
        const token = await createToken.mutateAsync({
          name: parsed.data.name,
          scope: parsed.data.scope,
          ...(expiresAt ? { expires_at: expiresAt } : {}),
        })
        setCreated(token)
        createForm.reset()
      } catch (err) {
        setTokenError(messageOf(err))
      }
    },
  })

  const passwordForm = useForm({
    defaultValues: { old: '', new: '' },
    onSubmit: async ({ value }) => {
      setPasswordError('')
      const parsed = passwordSchema.safeParse(value)
      if (!parsed.success) {
        setPasswordError(parsed.error.issues[0]?.message ?? 'Invalid input')
        return
      }
      try {
        await changePassword.mutateAsync(parsed.data)
      } catch (err) {
        setPasswordError(messageOf(err))
        return
      }
      // Every Session was revoked; the login page starts a fresh one.
      forgetCurrentSession()
      window.location.assign('/login')
    },
  })

  return (
    <div className="flex flex-col gap-8">
      <div>
        <h1 className="text-2xl font-semibold">Account</h1>
        <p className="text-sm text-muted-foreground">API tokens and browser Sessions for {user.name}.</p>
      </div>

      <section className="flex flex-col gap-4">
        <div>
          <h2 className="text-lg font-medium">My tokens &amp; Sessions</h2>
          <p className="text-sm text-muted-foreground">
            Tokens authenticate the CLI and API; Sessions are browser logins.
          </p>
        </div>

        {created && (
          <div className="rounded-md border border-border p-4">
            <p className="mb-2 text-sm font-medium">
              Token <span className="font-mono">{created.name}</span> created
            </p>
            <TokenReveal
              token={created.token}
              hint="This token appears only once. Store it before closing this panel."
              onDone={() => setCreated(null)}
            />
          </div>
        )}

        <form
          className="flex flex-wrap items-end gap-3 rounded-md border border-border p-4"
          onSubmit={(e) => {
            e.preventDefault()
            createForm.handleSubmit()
          }}
        >
          <createForm.Field name="name">
            {(field) => (
              <div className="flex flex-col gap-2">
                <Label htmlFor={field.name}>Name</Label>
                <Input
                  id={field.name}
                  value={field.state.value}
                  onChange={(e) => field.handleChange(e.target.value)}
                  placeholder="laptop-cli"
                  autoComplete="off"
                />
              </div>
            )}
          </createForm.Field>
          <createForm.Field name="scope">
            {(field) => (
              <div className="flex flex-col gap-2">
                <Label htmlFor={field.name}>Scope</Label>
                <select
                  id={field.name}
                  className="h-9 rounded-md border border-input bg-transparent px-3 text-sm"
                  value={field.state.value}
                  onChange={(e) => field.handleChange(e.target.value as NewToken['scope'])}
                >
                  {scopeOptions.map((scope) => (
                    <option key={scope} value={scope}>
                      {scope}
                    </option>
                  ))}
                </select>
              </div>
            )}
          </createForm.Field>
          <createForm.Field name="expires_at">
            {(field) => (
              <div className="flex flex-col gap-2">
                <Label htmlFor={field.name}>Expires (optional)</Label>
                <Input
                  id={field.name}
                  type="datetime-local"
                  value={field.state.value}
                  onChange={(e) => field.handleChange(e.target.value)}
                />
              </div>
            )}
          </createForm.Field>
          <Button type="submit" disabled={createToken.isPending}>
            {createToken.isPending ? 'Creating…' : 'Create token'}
          </Button>
        </form>

        {tokenError && <p className="text-sm text-destructive">{tokenError}</p>}
        {tokens.isPending && <p className="text-sm text-muted-foreground">Loading…</p>}
        {tokens.error && (
          <p className="text-sm text-destructive">Could not load tokens: {tokens.error.message}</p>
        )}

        {tokens.data?.items.length === 0 && (
          <p className="text-sm text-muted-foreground">No tokens or Sessions.</p>
        )}

        {tokens.data && tokens.data.items.length > 0 && (
          <ul className="divide-y divide-border rounded-md border border-border">
            {tokens.data.items.map((token) => (
              <li key={token.id} className="flex flex-wrap items-center justify-between gap-3 p-4">
                <div className="flex flex-col gap-1">
                  <div className="flex flex-wrap items-center gap-2">
                    <span className="font-medium">{token.name}</span>
                    <span className="rounded-full border border-border px-2 py-0.5 text-xs font-medium">
                      {token.kind === 'session' ? 'Session' : 'Token'}
                    </span>
                    {matchesCurrentSession(token, currentId) && (
                      <span
                        data-testid="current-session"
                        className="rounded-full border border-border bg-muted px-2 py-0.5 text-xs font-medium"
                      >
                        current
                      </span>
                    )}
                  </div>
                  <span className="text-xs text-muted-foreground">
                    {token.scope} · expires {formatTimestamp(token.expires_at)} · last used{' '}
                    {formatTimestamp(token.last_used_at)}
                  </span>
                </div>
                <Button
                  variant="outline"
                  size="sm"
                  disabled={revokeToken.isPending}
                  onClick={() => revokeToken.mutate(token.id)}
                >
                  Revoke
                </Button>
              </li>
            ))}
          </ul>
        )}
      </section>

      {user.login_type === 'password' && (
        <section className="flex flex-col gap-4">
          <div>
            <h2 className="text-lg font-medium">Change password</h2>
            <p className="text-sm text-muted-foreground">
              Changing the password signs out every Session, including this one.
            </p>
          </div>
          <form
            className="flex flex-wrap items-end gap-3 rounded-md border border-border p-4"
            onSubmit={(e) => {
              e.preventDefault()
              passwordForm.handleSubmit()
            }}
          >
            <passwordForm.Field name="old">
              {(field) => (
                <div className="flex flex-col gap-2">
                  <Label htmlFor={field.name}>Current password</Label>
                  <Input
                    id={field.name}
                    type="password"
                    value={field.state.value}
                    onChange={(e) => field.handleChange(e.target.value)}
                    autoComplete="current-password"
                  />
                </div>
              )}
            </passwordForm.Field>
            <passwordForm.Field name="new">
              {(field) => (
                <div className="flex flex-col gap-2">
                  <Label htmlFor={field.name}>New password</Label>
                  <Input
                    id={field.name}
                    type="password"
                    value={field.state.value}
                    onChange={(e) => field.handleChange(e.target.value)}
                    autoComplete="new-password"
                  />
                </div>
              )}
            </passwordForm.Field>
            <Button type="submit" disabled={changePassword.isPending}>
              {changePassword.isPending ? 'Changing…' : 'Change password'}
            </Button>
          </form>
          {passwordError && <p className="text-sm text-destructive">{passwordError}</p>}
        </section>
      )}
    </div>
  )
}
