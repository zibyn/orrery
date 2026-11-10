import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { createFileRoute } from '@tanstack/react-router'
import { KeyRound, Pencil, Plus, Trash } from 'lucide-react'
import { useState, type FormEvent } from 'react'

import { api, type ListResponse, type Secret } from '@/api'
import { PageHeader } from '@/components/page-header'
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
import { Button } from '@/components/ui/button'
import {
  Empty,
  EmptyDescription,
  EmptyHeader,
  EmptyMedia,
  EmptyTitle,
} from '@/components/ui/empty'
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
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetFooter,
  SheetHeader,
  SheetTitle,
} from '@/components/ui/sheet'
import { Spinner } from '@/components/ui/spinner'
import { useLocale } from '@/lib/i18n'
import { secretNameError } from '@/lib/secrets'
import { formatTimestamp } from '@/lib/tokens'
import { m } from '@/paraglide/messages'

export const Route = createFileRoute('/_authed/secrets')({
  component: SecretsPage,
})

function SecretsPage() {
  useLocale() // re-render on a language switch (see lib/i18n.tsx)
  const queryClient = useQueryClient()
  const [name, setName] = useState('')
  const [value, setValue] = useState('')
  const [error, setError] = useState('')
  const [open, setOpen] = useState(false)
  // Set when the drawer replaces an existing Secret: the name is fixed.
  const [replacing, setReplacing] = useState(false)
  // The row whose delete is being confirmed.
  const [confirming, setConfirming] = useState<string | null>(null)

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
  })

  const remove = useMutation({
    mutationFn: (secretName: string) =>
      api<Secret>('DELETE', `/secrets/${encodeURIComponent(secretName)}`),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ['secrets'] })
    },
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

  const openDrawer = (secretName = '') => {
    setName(secretName)
    setValue('')
    setError('')
    setReplacing(secretName !== '')
    setOpen(true)
  }

  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        title={m.secrets_title()}
        description={m.secrets_description()}
        actions={
          <Button onClick={() => openDrawer()}>
            <Plus data-icon="inline-start" />
            {m.secrets_add()}
          </Button>
        }
      />

      <Sheet open={open} onOpenChange={setOpen}>
        <SheetContent>
          <SheetHeader>
            <SheetTitle>
              {replacing ? m.secrets_replace_title({ name }) : m.secrets_add_title()}
            </SheetTitle>
            <SheetDescription>
              {replacing ? m.secrets_replace_description() : m.secrets_add_description()}
            </SheetDescription>
          </SheetHeader>
          <form id="secret-form" onSubmit={submit} className="flex-1 overflow-y-auto px-4">
            <FieldGroup>
              <Field>
                <FieldLabel htmlFor="secret-name">{m.common_name()}</FieldLabel>
                <Input
                  id="secret-name"
                  value={name}
                  onChange={(e) => setName(e.target.value)}
                  placeholder="GITHUB_TOKEN"
                  autoComplete="off"
                  readOnly={replacing}
                  className={replacing ? 'bg-muted text-muted-foreground' : undefined}
                  autoFocus={!replacing}
                />
              </Field>
              <Field>
                <FieldLabel htmlFor="secret-value">{m.secrets_value()}</FieldLabel>
                <Input
                  id="secret-value"
                  type="password"
                  value={value}
                  autoFocus={replacing}
                  onChange={(e) => setValue(e.target.value)}
                  autoComplete="new-password"
                />
              </Field>
              <FieldError className="whitespace-pre-line">{error}</FieldError>
            </FieldGroup>
          </form>
          <SheetFooter>
            <Button type="submit" form="secret-form" disabled={save.isPending}>
              {save.isPending ? m.secrets_saving() : m.secrets_save()}
            </Button>
            <Button type="button" variant="outline" onClick={() => setOpen(false)}>
              {m.common_cancel()}
            </Button>
          </SheetFooter>
        </SheetContent>
      </Sheet>

      {secrets.isPending && (
        <div className="flex items-center gap-2 py-8 text-sm text-muted-foreground">
          <Spinner />
          {m.secrets_loading()}
        </div>
      )}

      {secrets.data?.items.length === 0 && (
        <Empty className="border">
          <EmptyHeader>
            <EmptyMedia variant="icon">
              <KeyRound />
            </EmptyMedia>
            <EmptyTitle>{m.secrets_empty_title()}</EmptyTitle>
            <EmptyDescription>{m.secrets_empty_description()}</EmptyDescription>
          </EmptyHeader>
        </Empty>
      )}

      {secrets.data && secrets.data.items.length > 0 && (
        <ItemGroup className="gap-2">
          {secrets.data.items.map((secret) => (
            <Item key={secret.name} variant="outline" role="listitem">
              <ItemMedia variant="icon">
                <KeyRound />
              </ItemMedia>
              <ItemContent>
                <ItemTitle className="font-mono">{secret.name}</ItemTitle>
                <ItemDescription>
                  {m.secrets_updated({ time: formatTimestamp(secret.updated_at) })}
                </ItemDescription>
              </ItemContent>
              <ItemActions>
                <Button variant="outline" size="sm" onClick={() => openDrawer(secret.name)}>
                  <Pencil data-icon="inline-start" />
                  {m.secrets_replace()}
                </Button>
                <AlertDialog open={confirming === secret.name} onOpenChange={(next) => setConfirming(next ? secret.name : null)}>
                  <AlertDialogTrigger
                    render={<Button variant="ghost" size="sm" disabled={remove.isPending} />}
                  >
                    <Trash data-icon="inline-start" />
                    {m.common_delete()}
                  </AlertDialogTrigger>
                  <AlertDialogContent>
                    <AlertDialogHeader>
                      <AlertDialogTitle>{m.secret_delete_title({ name: secret.name })}</AlertDialogTitle>
                      <AlertDialogDescription>{m.secret_delete_description()}</AlertDialogDescription>
                    </AlertDialogHeader>
                    <AlertDialogFooter>
                      <AlertDialogCancel>{m.common_cancel()}</AlertDialogCancel>
                      <AlertDialogAction
                        variant="destructive"
                        data-testid="confirm-action"
                        onClick={() => {
                          setConfirming(null)
                          remove.mutate(secret.name)
                        }}
                      >
                        {m.common_delete()}
                      </AlertDialogAction>
                    </AlertDialogFooter>
                  </AlertDialogContent>
                </AlertDialog>
              </ItemActions>
            </Item>
          ))}
        </ItemGroup>
      )}
    </div>
  )
}
