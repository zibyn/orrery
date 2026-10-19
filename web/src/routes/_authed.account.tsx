import { useForm } from '@tanstack/react-form'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { createFileRoute } from '@tanstack/react-router'
import { KeyRound, Plus } from 'lucide-react'
import { useState, type ReactNode } from 'react'
import { z } from 'zod'

import { api, type CreatedToken, type ListResponse, type Token, type User } from '@/api'
import { PageHeader } from '@/components/page-header'
import { Problem } from '@/components/problem'
import { TokenReveal } from '@/components/token-reveal'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { matchesCurrentSession, readCurrentSessionId, forgetCurrentSession } from '@/lib/current-session'
import { useLocale } from '@/lib/i18n'
import { kindLabel, messageOf, scopeLabel } from '@/lib/labels'
import { expiresAtFromInput, formatTimestamp, scopeOptions } from '@/lib/tokens'
import { selectClass } from '@/lib/utils'
import { m } from '@/paraglide/messages'

const tokenSchema = z.object({
  name: z.string().trim().min(1, { error: () => m.account_name_required() }),
  scope: z.enum(['read_only', 'user', 'admin']),
  expires_at: z.string(),
})

const passwordSchema = z.object({
  old: z.string().min(1, { error: () => m.account_password_required() }),
  new: z.string().min(12, { error: () => m.account_password_min() }),
})

type NewToken = {
  name: string
  scope: (typeof scopeOptions)[number]
  expires_at?: string
}

function Field({ label, htmlFor, children }: { label: string; htmlFor: string; children: ReactNode }) {
  return (
    <div className="flex flex-col gap-1.5">
      <Label htmlFor={htmlFor}>{label}</Label>
      {children}
    </div>
  )
}

export const Route = createFileRoute('/_authed/account')({
  component: AccountPage,
})

function AccountPage() {
  useLocale() // re-render on a language switch (see lib/i18n.tsx)
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
        setTokenError(parsed.error.issues[0]?.message ?? m.common_invalid_input())
        return
      }
      const expiresAt = expiresAtFromInput(parsed.data.expires_at)
      if (parsed.data.expires_at && !expiresAt) {
        setTokenError(m.account_bad_expiry())
        return
      }
      if (expiresAt && new Date(expiresAt) <= new Date()) {
        setTokenError(m.account_expiry_future())
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
        setPasswordError(parsed.error.issues[0]?.message ?? m.common_invalid_input())
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
    <div className="flex max-w-3xl flex-col gap-6">
      <PageHeader
        title={m.account_title()}
        description={m.account_description({ name: user.name })}
      />

      {created && (
        <Card className="border-brand/30 bg-brand/5">
          <CardHeader>
            <CardTitle>{m.account_token_created_title({ name: created.name })}</CardTitle>
            <CardDescription>{m.account_token_created_description()}</CardDescription>
          </CardHeader>
          <CardContent>
            <TokenReveal token={created.token} onDone={() => setCreated(null)} />
          </CardContent>
        </Card>
      )}

      <Card>
        <CardHeader>
          <CardTitle>{m.account_create_token()}</CardTitle>
          <CardDescription>{m.account_create_token_description()}</CardDescription>
        </CardHeader>
        <CardContent>
          <form
            className="flex flex-wrap items-end gap-3"
            onSubmit={(e) => {
              e.preventDefault()
              createForm.handleSubmit()
            }}
          >
            <createForm.Field name="name">
              {(field) => (
                <Field label={m.common_name()} htmlFor={field.name}>
                  <Input
                    id={field.name}
                    className="w-44"
                    value={field.state.value}
                    onChange={(e) => field.handleChange(e.target.value)}
                    placeholder="laptop-cli"
                    autoComplete="off"
                  />
                </Field>
              )}
            </createForm.Field>
            <createForm.Field name="scope">
              {(field) => (
                <Field label={m.account_scope()} htmlFor={field.name}>
                  <select
                    id={field.name}
                    className={`${selectClass} w-32`}
                    value={field.state.value}
                    onChange={(e) => field.handleChange(e.target.value as NewToken['scope'])}
                  >
                    {scopeOptions.map((scope) => (
                      <option key={scope} value={scope}>
                        {scopeLabel(scope)}
                      </option>
                    ))}
                  </select>
                </Field>
              )}
            </createForm.Field>
            <createForm.Field name="expires_at">
              {(field) => (
                <Field label={m.account_expires()} htmlFor={field.name}>
                  <Input
                    id={field.name}
                    type="datetime-local"
                    className="w-52"
                    value={field.state.value}
                    onChange={(e) => field.handleChange(e.target.value)}
                  />
                </Field>
              )}
            </createForm.Field>
            <Button type="submit" disabled={createToken.isPending}>
              <Plus />
              {createToken.isPending ? m.account_creating() : m.account_create_token()}
            </Button>
          </form>
          {tokenError && (
            <p className="mt-3 text-sm whitespace-pre-line text-destructive" role="alert">
              {tokenError}
            </p>
          )}
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>{m.account_my_tokens()}</CardTitle>
          <CardDescription>{m.account_my_tokens_description()}</CardDescription>
        </CardHeader>
        <CardContent>
          {tokens.isPending && (
            <p className="text-sm text-muted-foreground">{m.common_loading()}</p>
          )}
          {tokens.error && (
            <p className="text-sm text-destructive" role="alert">
              {m.account_tokens_load_error()}: <Problem error={tokens.error} />
            </p>
          )}
          {tokens.data?.items.length === 0 && (
            <p className="text-sm text-muted-foreground">{m.account_no_tokens()}</p>
          )}

          {tokens.data && tokens.data.items.length > 0 && (
            <ul className="divide-y divide-border overflow-hidden rounded-lg border border-border">
              {tokens.data.items.map((token) => (
                <li key={token.id} className="flex items-center gap-3 px-3 py-2.5">
                  <span className="flex size-8 shrink-0 items-center justify-center rounded-lg border border-border bg-muted/50 text-muted-foreground">
                    <KeyRound className="size-4" />
                  </span>
                  <div className="min-w-0 flex-1">
                    <div className="flex flex-wrap items-center gap-2">
                      <span className="truncate text-sm font-medium">{token.name}</span>
                      <Badge variant={token.kind === 'session' ? 'secondary' : 'outline'}>
                        {kindLabel(token.kind)}
                      </Badge>
                      {matchesCurrentSession(token, currentId) && (
                        <Badge variant="success" data-testid="current-session">
                          {m.common_current()}
                        </Badge>
                      )}
                    </div>
                    <p className="truncate text-xs text-muted-foreground">
                      {m.account_token_meta({
                        scope: scopeLabel(token.scope),
                        expires: formatTimestamp(token.expires_at),
                        lastUsed: formatTimestamp(token.last_used_at),
                      })}
                    </p>
                  </div>
                  <Button
                    variant="ghost"
                    size="sm"
                    disabled={revokeToken.isPending}
                    onClick={() => revokeToken.mutate(token.id)}
                  >
                    {m.account_revoke()}
                  </Button>
                </li>
              ))}
            </ul>
          )}
        </CardContent>
      </Card>

      {user.login_type === 'password' && (
        <Card>
          <CardHeader>
            <CardTitle>{m.account_change_password()}</CardTitle>
            <CardDescription>{m.account_change_password_description()}</CardDescription>
          </CardHeader>
          <CardContent>
            <form
              className="flex flex-wrap items-end gap-3"
              onSubmit={(e) => {
                e.preventDefault()
                passwordForm.handleSubmit()
              }}
            >
              <passwordForm.Field name="old">
                {(field) => (
                  <Field label={m.account_current_password()} htmlFor={field.name}>
                    <Input
                      id={field.name}
                      type="password"
                      value={field.state.value}
                      onChange={(e) => field.handleChange(e.target.value)}
                      autoComplete="current-password"
                    />
                  </Field>
                )}
              </passwordForm.Field>
              <passwordForm.Field name="new">
                {(field) => (
                  <Field label={m.account_new_password()} htmlFor={field.name}>
                    <Input
                      id={field.name}
                      type="password"
                      value={field.state.value}
                      onChange={(e) => field.handleChange(e.target.value)}
                      autoComplete="new-password"
                    />
                  </Field>
                )}
              </passwordForm.Field>
              <Button type="submit" disabled={changePassword.isPending}>
                {changePassword.isPending ? m.account_changing() : m.account_change_password()}
              </Button>
            </form>
            {passwordError && (
              <p className="mt-3 text-sm whitespace-pre-line text-destructive" role="alert">
                {passwordError}
              </p>
            )}
          </CardContent>
        </Card>
      )}
    </div>
  )
}
