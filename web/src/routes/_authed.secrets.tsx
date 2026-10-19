import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { createFileRoute } from '@tanstack/react-router'
import { KeyRound, Trash } from 'lucide-react'
import { useState, type FormEvent, type ReactNode } from 'react'

import { api, type ListResponse, type Secret } from '@/api'
import { EmptyState } from '@/components/empty-state'
import { PageHeader } from '@/components/page-header'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { secretNameError } from '@/lib/secrets'
import { formatTimestamp } from '@/lib/tokens'
import { messageOf } from '@/lib/utils'

export const Route = createFileRoute('/_authed/secrets')({
  component: SecretsPage,
})

function Field({ label, htmlFor, children }: { label: string; htmlFor: string; children: ReactNode }) {
  return (
    <div className="flex flex-col gap-1.5">
      <Label htmlFor={htmlFor}>{label}</Label>
      {children}
    </div>
  )
}

function SecretsPage() {
  const queryClient = useQueryClient()
  const [name, setName] = useState('')
  const [value, setValue] = useState('')
  const [error, setError] = useState('')

  const secrets = useQuery({
    queryKey: ['secrets'],
    queryFn: () => api<ListResponse<Secret>>('GET', '/secrets'),
  })

  const save = useMutation({
    mutationFn: (payload: { name: string; value: string }) =>
      api<Secret>('PUT', `/secrets/${encodeURIComponent(payload.name)}`, {
        value: payload.value,
      }),
    onSuccess: () => {
      setValue('')
      setError('')
      void queryClient.invalidateQueries({ queryKey: ['secrets'] })
    },
    onError: (err) => setError(messageOf(err)),
  })

  const remove = useMutation({
    mutationFn: (secretName: string) =>
      api<Secret>('DELETE', `/secrets/${encodeURIComponent(secretName)}`),
    onSuccess: () => {
      setError('')
      void queryClient.invalidateQueries({ queryKey: ['secrets'] })
    },
    onError: (err) => setError(messageOf(err)),
  })

  function submit(event: FormEvent) {
    event.preventDefault()
    const nameError = secretNameError(name)
    if (nameError) {
      setError(nameError)
      return
    }
    if (!value) {
      setError('Enter a value')
      return
    }
    save.mutate({ name: name.trim(), value })
  }

  return (
    <div className="flex max-w-3xl flex-col gap-6">
      <PageHeader
        title="Secrets"
        description="Values are write-only: the console never shows a stored value. Saving an existing name overwrites it."
      />

      <Card>
        <CardHeader>
          <CardTitle>Add or overwrite</CardTitle>
          <CardDescription>The name is the key injected where the Secret is used.</CardDescription>
        </CardHeader>
        <CardContent>
          <form onSubmit={submit} className="flex flex-wrap items-end gap-3">
            <Field label="Name" htmlFor="secret-name">
              <Input
                id="secret-name"
                className="w-56"
                value={name}
                onChange={(e) => setName(e.target.value)}
                placeholder="GITHUB_TOKEN"
                autoComplete="off"
              />
            </Field>
            <div className="flex min-w-56 flex-1 flex-col gap-1.5">
              <Label htmlFor="secret-value">Value</Label>
              <Input
                id="secret-value"
                type="password"
                value={value}
                onChange={(e) => setValue(e.target.value)}
                autoComplete="new-password"
              />
            </div>
            <Button type="submit" disabled={save.isPending}>
              {save.isPending ? 'Saving…' : 'Save Secret'}
            </Button>
          </form>
          {error && (
            <p className="mt-3 text-sm text-destructive" role="alert">
              {error}
            </p>
          )}
        </CardContent>
      </Card>

      {secrets.isPending && (
        <p className="py-8 text-sm text-muted-foreground">Loading secrets…</p>
      )}
      {secrets.error && (
        <p className="text-sm text-destructive" role="alert">
          Could not load Secrets: {secrets.error.message}
        </p>
      )}

      {secrets.data?.items.length === 0 && (
        <EmptyState
          icon={<KeyRound />}
          title="No Secrets yet"
          description="Secret values can only be replaced, never read back."
        />
      )}

      {secrets.data && secrets.data.items.length > 0 && (
        <div className="divide-y divide-border overflow-hidden rounded-xl border border-border bg-card">
          {secrets.data.items.map((secret) => (
            <div key={secret.name} className="flex items-center gap-3 px-4 py-3">
              <span className="flex size-8 shrink-0 items-center justify-center rounded-lg border border-border bg-muted/50 text-muted-foreground">
                <KeyRound className="size-4" />
              </span>
              <div className="min-w-0 flex-1">
                <p className="truncate font-mono text-sm font-medium">{secret.name}</p>
                <p className="text-xs text-muted-foreground">
                  Updated {formatTimestamp(secret.updated_at)}
                </p>
              </div>
              <Button
                variant="ghost"
                size="sm"
                disabled={remove.isPending}
                onClick={() => remove.mutate(secret.name)}
              >
                <Trash />
                Delete
              </Button>
            </div>
          ))}
        </div>
      )}
    </div>
  )
}
