import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { createFileRoute } from '@tanstack/react-router'
import { KeyRound, Plus, Trash } from 'lucide-react'
import { useState, type FormEvent, type ReactNode } from 'react'

import { api, type ListResponse, type Secret } from '@/api'
import { EmptyState } from '@/components/empty-state'
import { ListCard, ListRow } from '@/components/list'
import { PageHeader } from '@/components/page-header'
import { Problem } from '@/components/problem'
import { Button } from '@/components/ui/button'
import { Drawer } from '@/components/ui/drawer'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { useLocale } from '@/lib/i18n'
import { messageOf } from '@/lib/labels'
import { secretNameError } from '@/lib/secrets'
import { formatTimestamp } from '@/lib/tokens'
import { m } from '@/paraglide/messages'

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
  useLocale() // re-render on a language switch (see lib/i18n.tsx)
  const queryClient = useQueryClient()
  const [name, setName] = useState('')
  const [value, setValue] = useState('')
  const [error, setError] = useState('')
  const [removeError, setRemoveError] = useState('')
  const [open, setOpen] = useState(false)

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
      setName('')
      setValue('')
      setError('')
      setOpen(false)
      void queryClient.invalidateQueries({ queryKey: ['secrets'] })
    },
    onError: (err) => setError(messageOf(err)),
  })

  const remove = useMutation({
    mutationFn: (secretName: string) =>
      api<Secret>('DELETE', `/secrets/${encodeURIComponent(secretName)}`),
    onSuccess: () => {
      setRemoveError('')
      void queryClient.invalidateQueries({ queryKey: ['secrets'] })
    },
    onError: (err) => setRemoveError(messageOf(err)),
  })

  function submit(event: FormEvent) {
    event.preventDefault()
    const nameError = secretNameError(name)
    if (nameError) {
      setError(nameError)
      return
    }
    if (!value) {
      setError(m.secrets_enter_value())
      return
    }
    save.mutate({ name: name.trim(), value })
  }

  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        title={m.secrets_title()}
        description={m.secrets_description()}
        actions={
          <Button
            onClick={() => {
              setError('')
              setOpen(true)
            }}
          >
            <Plus />
            {m.secrets_add()}
          </Button>
        }
      />

      <Drawer
        open={open}
        onOpenChange={setOpen}
        title={m.secrets_add_title()}
        description={m.secrets_add_description()}
        footer={
          <>
            <Button type="button" variant="ghost" onClick={() => setOpen(false)}>
              {m.common_cancel()}
            </Button>
            <Button type="submit" form="secret-form" disabled={save.isPending}>
              {save.isPending ? m.secrets_saving() : m.secrets_save()}
            </Button>
          </>
        }
      >
        <form id="secret-form" onSubmit={submit} className="flex flex-col gap-4">
          <Field label={m.common_name()} htmlFor="secret-name">
            <Input
              id="secret-name"
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="GITHUB_TOKEN"
              autoComplete="off"
            />
          </Field>
          <Field label={m.secrets_value()} htmlFor="secret-value">
            <Input
              id="secret-value"
              type="password"
              value={value}
              onChange={(e) => setValue(e.target.value)}
              autoComplete="new-password"
            />
          </Field>
          {error && (
            <p className="text-sm whitespace-pre-line text-destructive" role="alert">
              {error}
            </p>
          )}
        </form>
      </Drawer>

      {secrets.isPending && (
        <p className="py-8 text-sm text-muted-foreground">{m.secrets_loading()}</p>
      )}
      {secrets.error && (
        <p className="text-sm text-destructive" role="alert">
          {m.secrets_load_error()}: <Problem error={secrets.error} />
        </p>
      )}
      {removeError && (
        <p className="text-sm text-destructive" role="alert">
          {removeError}
        </p>
      )}

      {secrets.data?.items.length === 0 && (
        <EmptyState
          icon={<KeyRound />}
          title={m.secrets_empty_title()}
          description={m.secrets_empty_description()}
        />
      )}

      {secrets.data && secrets.data.items.length > 0 && (
        <ListCard>
          {secrets.data.items.map((secret) => (
            <ListRow
              key={secret.name}
              icon={<KeyRound />}
              title={<span className="truncate font-mono text-sm font-medium">{secret.name}</span>}
              description={m.secrets_updated({ time: formatTimestamp(secret.updated_at) })}
              trailing={
                <Button
                  variant="ghost"
                  size="sm"
                  disabled={remove.isPending}
                  onClick={() => remove.mutate(secret.name)}
                >
                  <Trash />
                  {m.common_delete()}
                </Button>
              }
            />
          ))}
        </ListCard>
      )}
    </div>
  )
}
