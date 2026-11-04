import { useForm } from '@tanstack/react-form'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { createFileRoute } from '@tanstack/react-router'
import { KeyRound, Monitor, Plus } from 'lucide-react'
import { useState } from 'react'
import { z } from 'zod'

import { api, type CreatedToken, type ListResponse, type Token, type User } from '@/api'
import { ConfirmButton } from '@/components/confirm-button'
import { Field } from '@/components/field'
import { ListCard, ListRow } from '@/components/list'
import { PageHeader } from '@/components/page-header'
import { Problem } from '@/components/problem'
import { TokenReveal } from '@/components/token-reveal'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Drawer } from '@/components/ui/drawer'
import { Input } from '@/components/ui/input'
import { matchesCurrentSession, readCurrentSessionId, forgetCurrentSession } from '@/lib/current-session'
import { useLocale } from '@/lib/i18n'
import { messageOf, scopeLabel } from '@/lib/labels'
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
  const [tokenOpen, setTokenOpen] = useState(false)
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
    onSuccess: (_, id) => {
      if (id === currentId) {
        forgetCurrentSession()
        window.location.assign('/login')
        return
      }
      setTokenError('')
      void queryClient.invalidateQueries({ queryKey: ['tokens'] })
    },
    onError: (err) => setTokenError(messageOf(err)),
  })

  const apiTokens = (tokens.data?.items ?? []).filter((token) => token.kind === 'token')
  // The browser in use first, then the most recent sign-ins.
  const sessions = (tokens.data?.items ?? [])
    .filter((token) => token.kind === 'session')
    .sort(
      (a, b) =>
        Number(matchesCurrentSession(b, currentId)) - Number(matchesCurrentSession(a, currentId)) ||
        b.created_at.localeCompare(a.created_at),
    )

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
        setTokenOpen(false)
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
        actions={
          <Button
            onClick={() => {
              setTokenError('')
              setTokenOpen(true)
            }}
          >
            <Plus />
            {m.account_create_token()}
          </Button>
        }
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

      <Drawer
        open={tokenOpen}
        onOpenChange={setTokenOpen}
        title={m.account_create_token()}
        description={m.account_create_token_description()}
        footer={
          <>
            <Button type="button" variant="ghost" onClick={() => setTokenOpen(false)}>
              {m.common_cancel()}
            </Button>
            <Button type="submit" form="create-token-form" disabled={createToken.isPending}>
              <Plus />
              {createToken.isPending ? m.account_creating() : m.common_create()}
            </Button>
          </>
        }
      >
        <form
          id="create-token-form"
          className="flex flex-col gap-4"
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
                  className={selectClass}
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
                  value={field.state.value}
                  onChange={(e) => field.handleChange(e.target.value)}
                />
              </Field>
            )}
          </createForm.Field>
          {tokenError && (
            <p className="text-sm whitespace-pre-line text-destructive" role="alert">
              {tokenError}
            </p>
          )}
        </form>
      </Drawer>

      {tokens.isPending && <p className="text-sm text-muted-foreground">{m.common_loading()}</p>}
      {tokens.error && (
        <p className="text-sm text-destructive" role="alert">
          {m.account_tokens_load_error()}: <Problem error={tokens.error} />
        </p>
      )}
      {tokenError && !tokenOpen && (
        <p className="text-sm whitespace-pre-line text-destructive" role="alert">
          {tokenError}
        </p>
      )}

      {tokens.data && (
        <section className="flex flex-col gap-3">
          <div>
            <h2 className="text-sm font-semibold tracking-tight">{m.account_tokens()}</h2>
            <p className="text-xs text-muted-foreground">{m.account_tokens_description()}</p>
          </div>
          {apiTokens.length === 0 ? (
            <p className="text-sm text-muted-foreground">{m.account_no_api_tokens()}</p>
          ) : (
            <ListCard>
              {apiTokens.map((token) => (
                <ListRow
                  key={token.id}
                  icon={<KeyRound />}
                  title={<span className="truncate text-sm font-medium">{token.name}</span>}
                  description={m.account_token_meta({
                    scope: scopeLabel(token.scope),
                    expires: token.expires_at ? formatTimestamp(token.expires_at) : m.account_never(),
                    lastUsed: formatTimestamp(token.last_used_at),
                  })}
                  trailing={
                    <ConfirmButton
                      size="sm"
                      title={m.token_revoke_title({ name: token.name })}
                      description={m.token_revoke_description()}
                      confirmLabel={m.account_revoke()}
                      disabled={revokeToken.isPending}
                      onConfirm={() => revokeToken.mutate(token.id)}
                    >
                      {m.account_revoke()}
                    </ConfirmButton>
                  }
                />
              ))}
            </ListCard>
          )}
        </section>
      )}

      {tokens.data && sessions.length > 0 && (
        <section className="flex flex-col gap-3">
          <div>
            <h2 className="text-sm font-semibold tracking-tight">{m.account_sessions()}</h2>
            <p className="text-xs text-muted-foreground">{m.account_sessions_description()}</p>
          </div>
          <ListCard>
            {sessions.map((session) => {
              const isCurrent = matchesCurrentSession(session, currentId)
              return (
                <ListRow
                  key={session.id}
                  icon={<Monitor />}
                  title={
                    <>
                      <span className="truncate text-sm font-medium">
                        {isCurrent ? m.account_this_browser() : m.account_other_browser()}
                      </span>
                      {isCurrent && (
                        <Badge variant="success" data-testid="current-session">
                          {m.common_current()}
                        </Badge>
                      )}
                    </>
                  }
                  description={m.account_session_meta({
                    created: formatTimestamp(session.created_at),
                    expires: session.expires_at ? formatTimestamp(session.expires_at) : m.account_never(),
                  })}
                  trailing={
                    <ConfirmButton
                      size="sm"
                      title={m.session_revoke_title()}
                      description={
                        isCurrent
                          ? m.session_revoke_current_description()
                          : m.session_revoke_description()
                      }
                      confirmLabel={m.account_sign_out_session()}
                      disabled={revokeToken.isPending}
                      onConfirm={() => revokeToken.mutate(session.id)}
                    >
                      {m.account_sign_out_session()}
                    </ConfirmButton>
                  }
                />
              )
            })}
          </ListCard>
        </section>
      )}

      {user.login_type === 'password' && (
        <Card>
          <CardHeader>
            <CardTitle>{m.account_change_password()}</CardTitle>
            <CardDescription>{m.account_change_password_description()}</CardDescription>
          </CardHeader>
          <CardContent>
            <form
              className="grid items-start gap-4 sm:grid-cols-2"
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
                  <Field
                    label={m.account_new_password()}
                    htmlFor={field.name}
                    hint={m.account_password_hint()}
                  >
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
              <div className="sm:col-span-2">
                <Button type="submit" disabled={changePassword.isPending}>
                  {changePassword.isPending ? m.account_changing() : m.account_change_password()}
                </Button>
              </div>
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
