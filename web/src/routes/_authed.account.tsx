import { useForm } from '@tanstack/react-form'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { createFileRoute } from '@tanstack/react-router'
import { KeyRound, Monitor, Plus, UserPen } from 'lucide-react'
import { useState } from 'react'
import { z } from 'zod'

import { api, type CreatedToken, type ListResponse, type Token } from '@/api'
import { PageHeader } from '@/components/page-header'
import { Problem } from '@/components/problem'
import { TokenReveal } from '@/components/token-reveal'
import { Alert, AlertDescription } from '@/components/ui/alert'
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
  AlertDialogTrigger,
} from '@/components/ui/alert-dialog'
import { Badge } from '@/components/ui/badge'
import { Button, buttonVariants } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Field, FieldError, FieldGroup, FieldLabel } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import {
  Item,
  ItemActions,
  ItemContent,
  ItemDescription,
  ItemGroup,
  ItemMedia,
  ItemTitle,
} from '@/components/ui/item'
import { NativeSelect, NativeSelectOption } from '@/components/ui/native-select'
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetFooter,
  SheetHeader,
  SheetTitle,
} from '@/components/ui/sheet'
import { Spinner } from '@/components/ui/spinner'
import { matchesCurrentSession, readCurrentSessionId, forgetCurrentSession } from '@/lib/current-session'
import { useLocale } from '@/lib/i18n'
import { messageOf, scopeLabel } from '@/lib/labels'
import { expiresAtFromInput, formatTimestamp, scopeOptions } from '@/lib/tokens'
import { cn } from '@/lib/utils'
import { m } from '@/paraglide/messages'

const tokenSchema = z.object({
  name: z.string().trim().min(1, { error: () => m.account_name_required() }),
  scope: z.enum(['read_only', 'user', 'admin']),
  expires_at: z.string(),
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
  // A Polaris login never learns its Session id, so normally no row is marked
  // (api.md §8); a stale stored id simply matches nothing.
  const [currentId] = useState(readCurrentSessionId)
  const [created, setCreated] = useState<CreatedToken | null>(null)
  const [tokenOpen, setTokenOpen] = useState(false)
  const [tokenError, setTokenError] = useState('')
  // The token or session whose revoke is being confirmed.
  const [confirming, setConfirming] = useState<string | null>(null)

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

  return (
    <div className="flex max-w-3xl flex-col gap-6">
      <PageHeader
        title={m.account_title()}
        description={m.account_description({ name: user.name })}
        actions={
          <>
            {/* Profile edits live in Polaris (ADR-0029); the server knows its URL. */}
            {!user.machine && (
              <a href="/api/v1/oidc/account" className={cn(buttonVariants({ variant: 'outline' }))}>
                <UserPen data-icon="inline-start" />
                {m.account_polaris_profile()}
              </a>
            )}
            <Button
              onClick={() => {
                setTokenError('')
                setTokenOpen(true)
              }}
            >
              <Plus data-icon="inline-start" />
              {m.account_create_token()}
            </Button>
          </>
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

      <Sheet open={tokenOpen} onOpenChange={setTokenOpen}>
        <SheetContent>
          <SheetHeader>
            <SheetTitle>{m.account_create_token()}</SheetTitle>
            <SheetDescription>{m.account_create_token_description()}</SheetDescription>
          </SheetHeader>
          <form
            id="create-token-form"
            className="flex-1 overflow-y-auto px-4"
            onSubmit={(e) => {
              e.preventDefault()
              createForm.handleSubmit()
            }}
          >
            <FieldGroup>
              <createForm.Field name="name">
                {(field) => (
                  <Field>
                    <FieldLabel htmlFor={field.name}>{m.common_name()}</FieldLabel>
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
                  <Field>
                    <FieldLabel htmlFor={field.name}>{m.account_scope()}</FieldLabel>
                    <NativeSelect
                      id={field.name}
                      className="w-full"
                      value={field.state.value}
                      onChange={(e) => field.handleChange(e.target.value as NewToken['scope'])}
                    >
                      {scopeOptions.map((scope) => (
                        <NativeSelectOption key={scope} value={scope}>
                          {scopeLabel(scope)}
                        </NativeSelectOption>
                      ))}
                    </NativeSelect>
                  </Field>
                )}
              </createForm.Field>
              <createForm.Field name="expires_at">
                {(field) => (
                  <Field>
                    <FieldLabel htmlFor={field.name}>{m.account_expires()}</FieldLabel>
                    <Input
                      id={field.name}
                      type="datetime-local"
                      value={field.state.value}
                      onChange={(e) => field.handleChange(e.target.value)}
                    />
                  </Field>
                )}
              </createForm.Field>
              <FieldError className="whitespace-pre-line">{tokenError}</FieldError>
            </FieldGroup>
          </form>
          <SheetFooter>
            <Button type="submit" form="create-token-form" disabled={createToken.isPending}>
              <Plus data-icon="inline-start" />
              {createToken.isPending ? m.account_creating() : m.common_create()}
            </Button>
            <Button type="button" variant="outline" onClick={() => setTokenOpen(false)}>
              {m.common_cancel()}
            </Button>
          </SheetFooter>
        </SheetContent>
      </Sheet>

      {tokens.isPending && (
        <div className="flex items-center gap-2 py-8 text-sm text-muted-foreground">
          <Spinner />
          {m.common_loading()}
        </div>
      )}
      {tokens.error && (
        <Alert variant="destructive">
          <AlertDescription>
            {m.account_tokens_load_error()}: <Problem error={tokens.error} />
          </AlertDescription>
        </Alert>
      )}
      {tokenError && !tokenOpen && (
        <Alert variant="destructive">
          <AlertDescription className="whitespace-pre-line">{tokenError}</AlertDescription>
        </Alert>
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
            <ItemGroup className="gap-2">
              {apiTokens.map((token) => (
                <Item key={token.id} variant="outline" role="listitem">
                  <ItemMedia variant="icon">
                    <KeyRound />
                  </ItemMedia>
                  <ItemContent>
                    <ItemTitle>{token.name}</ItemTitle>
                    <ItemDescription>
                      {m.account_token_meta({
                        scope: scopeLabel(token.scope),
                        expires: token.expires_at
                          ? formatTimestamp(token.expires_at)
                          : m.account_never(),
                        lastUsed: formatTimestamp(token.last_used_at),
                      })}
                    </ItemDescription>
                  </ItemContent>
                  <ItemActions>
                    <AlertDialog open={confirming === token.id} onOpenChange={(next) => setConfirming(next ? token.id : null)}>
                      <AlertDialogTrigger
                        render={<Button variant="ghost" size="sm" disabled={revokeToken.isPending} />}
                      >
                        {m.account_revoke()}
                      </AlertDialogTrigger>
                      <AlertDialogContent>
                        <AlertDialogHeader>
                          <AlertDialogTitle>{m.token_revoke_title({ name: token.name })}</AlertDialogTitle>
                          <AlertDialogDescription>{m.token_revoke_description()}</AlertDialogDescription>
                        </AlertDialogHeader>
                        <AlertDialogFooter>
                          <AlertDialogCancel>{m.common_cancel()}</AlertDialogCancel>
                          <AlertDialogAction
                            variant="destructive"
                            data-testid="confirm-action"
                            onClick={() => {
                              setConfirming(null)
                              revokeToken.mutate(token.id)
                            }}
                          >
                            {m.account_revoke()}
                          </AlertDialogAction>
                        </AlertDialogFooter>
                      </AlertDialogContent>
                    </AlertDialog>
                  </ItemActions>
                </Item>
              ))}
            </ItemGroup>
          )}
        </section>
      )}

      {tokens.data && sessions.length > 0 && (
        <section className="flex flex-col gap-3">
          <div>
            <h2 className="text-sm font-semibold tracking-tight">{m.account_sessions()}</h2>
            <p className="text-xs text-muted-foreground">{m.account_sessions_description()}</p>
          </div>
          <ItemGroup className="gap-2">
            {sessions.map((session) => {
              const isCurrent = matchesCurrentSession(session, currentId)
              return (
                <Item key={session.id} variant="outline" role="listitem">
                  <ItemMedia variant="icon">
                    <Monitor />
                  </ItemMedia>
                  <ItemContent>
                    <ItemTitle>
                      {isCurrent ? m.account_this_browser() : m.account_other_browser()}
                      {isCurrent && (
                        <Badge variant="success" data-testid="current-session">
                          {m.common_current()}
                        </Badge>
                      )}
                    </ItemTitle>
                    <ItemDescription>
                      {m.account_session_meta({
                        created: formatTimestamp(session.created_at),
                        expires: session.expires_at
                          ? formatTimestamp(session.expires_at)
                          : m.account_never(),
                      })}
                    </ItemDescription>
                  </ItemContent>
                  <ItemActions>
                    <AlertDialog open={confirming === session.id} onOpenChange={(next) => setConfirming(next ? session.id : null)}>
                      <AlertDialogTrigger
                        render={<Button variant="ghost" size="sm" disabled={revokeToken.isPending} />}
                      >
                        {m.account_sign_out_session()}
                      </AlertDialogTrigger>
                      <AlertDialogContent>
                        <AlertDialogHeader>
                          <AlertDialogTitle>{m.session_revoke_title()}</AlertDialogTitle>
                          <AlertDialogDescription>
                            {isCurrent
                              ? m.session_revoke_current_description()
                              : m.session_revoke_description()}
                          </AlertDialogDescription>
                        </AlertDialogHeader>
                        <AlertDialogFooter>
                          <AlertDialogCancel>{m.common_cancel()}</AlertDialogCancel>
                          <AlertDialogAction
                            variant="destructive"
                            data-testid="confirm-action"
                            onClick={() => {
                              setConfirming(null)
                              revokeToken.mutate(session.id)
                            }}
                          >
                            {m.account_sign_out_session()}
                          </AlertDialogAction>
                        </AlertDialogFooter>
                      </AlertDialogContent>
                    </AlertDialog>
                  </ItemActions>
                </Item>
              )
            })}
          </ItemGroup>
        </section>
      )}
    </div>
  )
}
