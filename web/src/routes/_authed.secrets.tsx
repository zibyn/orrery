import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { createFileRoute } from '@tanstack/react-router'
import { useState, type FormEvent } from 'react'

import { api, type ListResponse, type Secret } from '@/api'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { secretNameError } from '@/lib/secrets'
import { formatTimestamp } from '@/lib/tokens'

export const Route = createFileRoute('/_authed/secrets')({
  component: SecretsPage,
})

function messageOf(error: unknown): string {
  return error instanceof Error ? error.message : String(error)
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
    <div className="flex flex-col gap-6">
      <div>
        <h1 className="text-2xl font-semibold">Secrets</h1>
        <p className="text-sm text-muted-foreground">
          Values are write-only: the console never shows a stored value.
        </p>
      </div>

      <form onSubmit={submit} className="flex flex-wrap items-end gap-3 rounded-md border border-border p-4">
        <div className="flex flex-col gap-2">
          <Label htmlFor="secret-name">Name</Label>
          <Input
            id="secret-name"
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder="GITHUB_TOKEN"
            autoComplete="off"
          />
        </div>
        <div className="flex flex-1 flex-col gap-2">
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

      {error && <p className="text-sm text-destructive">{error}</p>}
      {secrets.isPending && <p className="text-sm text-muted-foreground">Loading…</p>}
      {secrets.error && (
        <p className="text-sm text-destructive">Could not load Secrets: {secrets.error.message}</p>
      )}

      {secrets.data?.items.length === 0 && (
        <p className="text-sm text-muted-foreground">No Secrets yet.</p>
      )}

      {secrets.data && secrets.data.items.length > 0 && (
        <ul className="divide-y divide-border rounded-md border border-border">
          {secrets.data.items.map((secret) => (
            <li
              key={secret.name}
              className="flex flex-wrap items-center justify-between gap-3 p-4"
            >
              <div className="flex flex-col gap-1">
                <span className="font-mono text-sm font-medium">{secret.name}</span>
                <span className="text-xs text-muted-foreground">
                  Updated {formatTimestamp(secret.updated_at)}
                </span>
              </div>
              <Button
                variant="outline"
                size="sm"
                disabled={remove.isPending}
                onClick={() => remove.mutate(secret.name)}
              >
                Delete
              </Button>
            </li>
          ))}
        </ul>
      )}
    </div>
  )
}
