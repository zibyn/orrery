import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { createFileRoute } from '@tanstack/react-router'
import { Pencil, ShieldCheck, Trash } from 'lucide-react'
import { useState } from 'react'

import { api, type ListResponse, type QuotaUsage, type User } from '@/api'
import { PageHeader } from '@/components/page-header'
import { Problem } from '@/components/problem'
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
import {
  Breadcrumb,
  BreadcrumbItem,
  BreadcrumbList,
  BreadcrumbPage,
  BreadcrumbSeparator,
} from '@/components/ui/breadcrumb'
import { Button } from '@/components/ui/button'
import {
  Field,
  FieldError,
  FieldGroup,
  FieldLabel,
  FieldLegend,
  FieldSet,
} from '@/components/ui/field'
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
  SheetHeader,
  SheetTitle,
} from '@/components/ui/sheet'
import { Spinner } from '@/components/ui/spinner'
import { useLocale } from '@/lib/i18n'
import { identityLabel } from '@/lib/labels'
import { m } from '@/paraglide/messages'

export const Route = createFileRoute('/_authed/admin/users')({
  component: UsersPage,
})

// People come from Polaris and machine Users from `orrery-server user-token`
// (ADR-0029), so the console only lists, sets quotas and deletes.
function UsersPage() {
  useLocale() // re-render on a language switch (see lib/i18n.tsx)
  const { user: me } = Route.useRouteContext()

  const users = useQuery({
    queryKey: ['users'],
    queryFn: () => api<ListResponse<User>>('GET', '/users'),
  })
  // The public API exposes per-User usage only for the caller (GET /users/me/quota);
  // another User's used counts are not available to the console.
  const myQuota = useQuery({
    queryKey: ['me', 'quota'],
    queryFn: () => api<QuotaUsage>('GET', '/users/me/quota'),
  })

  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        above={
          <Breadcrumb aria-label={m.common_breadcrumb()}>
            <BreadcrumbList>
              <BreadcrumbItem>{m.nav_group_admin()}</BreadcrumbItem>
              <BreadcrumbSeparator />
              <BreadcrumbItem>
                <BreadcrumbPage>{m.users_title()}</BreadcrumbPage>
              </BreadcrumbItem>
            </BreadcrumbList>
          </Breadcrumb>
        }
        title={m.users_title()}
        description={m.users_description()}
        meta={
          myQuota.data ? (
            <span data-testid="my-quota">
              {m.users_usage({
                gpusUsed: myQuota.data.gpus.used,
                gpusLimit: myQuota.data.gpus.limit,
                sandboxesUsed: myQuota.data.sandboxes.used,
                sandboxesLimit: myQuota.data.sandboxes.limit,
              })}
            </span>
          ) : undefined
        }
      />

      {users.isPending && (
        <div className="flex items-center gap-2 py-8 text-sm text-muted-foreground">
          <Spinner />
          {m.users_loading()}
        </div>
      )}
      {users.error && (
        <Alert variant="destructive">
          <AlertDescription>
            {m.users_load_error()}: <Problem error={users.error} />
          </AlertDescription>
        </Alert>
      )}

      {users.data && users.data.items.length > 0 && (
        <ItemGroup className="gap-2">
          {users.data.items.map((user) => (
            <UserRow key={user.id} user={user} isSelf={user.id === me.id} />
          ))}
        </ItemGroup>
      )}
    </div>
  )
}

function UserRow({ user, isSelf }: { user: User; isSelf: boolean }) {
  const queryClient = useQueryClient()
  const [editOpen, setEditOpen] = useState(false)
  const [gpus, setGpus] = useState(String(user.quota.gpus))
  const [sandboxes, setSandboxes] = useState(String(user.quota.sandboxes))
  const [confirmOpen, setConfirmOpen] = useState(false)

  const invalidate = () => queryClient.invalidateQueries({ queryKey: ['users'] })

  const save = useMutation({
    mutationFn: () =>
      api<User>('PUT', `/users/${user.id}/quota`, {
        gpus: Number(gpus),
        sandboxes: Number(sandboxes),
      }),
    onSuccess: async () => {
      await invalidate()
      setEditOpen(false)
    },
    onError: invalidate,
  })
  const remove = useMutation({
    mutationFn: () => api<User>('DELETE', `/users/${user.id}`),
    onSuccess: invalidate,
  })

  const openEditor = () => {
    // Start from the saved values, not from an abandoned earlier edit.
    setGpus(String(user.quota.gpus))
    setSandboxes(String(user.quota.sandboxes))
    save.reset()
    setEditOpen(true)
  }

  const quotaValid =
    Number.isInteger(Number(gpus)) &&
    Number(gpus) >= 0 &&
    Number.isInteger(Number(sandboxes)) &&
    Number(sandboxes) >= 0

  const idPrefix = `edit-user-${user.id}`

  return (
    <>
      <Item variant="outline" role="listitem" data-testid="user-row">
        <ItemMedia variant="icon">
          <span aria-hidden="true">{user.name.slice(0, 1).toUpperCase()}</span>
        </ItemMedia>
        <ItemContent>
          <ItemTitle>
            {user.name}
            {user.is_admin && (
              <Badge variant="secondary">
                <ShieldCheck data-icon="inline-start" />
                {m.users_badge_admin()}
              </Badge>
            )}
            {isSelf && <Badge variant="outline">{m.users_badge_you()}</Badge>}
            {user.deleted_at && <Badge variant="destructive">{m.users_badge_deleted()}</Badge>}
          </ItemTitle>
          <ItemDescription>
            {user.email ?? m.users_no_email()} ·{' '}
            <span data-testid="user-identity">{identityLabel(user)}</span> ·{' '}
            {m.users_limits({ gpus: user.quota.gpus, sandboxes: user.quota.sandboxes })}
          </ItemDescription>
          {remove.error && (
            <FieldError>
              <Problem error={remove.error} />
            </FieldError>
          )}
        </ItemContent>
        {!user.deleted_at && (
          <ItemActions>
            <Button variant="outline" size="sm" onClick={openEditor}>
              <Pencil data-icon="inline-start" />
              {m.common_edit()}
            </Button>
            {isSelf ? (
              <Button variant="ghost" size="sm" disabled title={m.users_cannot_delete_self()}>
                <Trash data-icon="inline-start" />
                {m.common_delete()}
              </Button>
            ) : (
              <AlertDialog open={confirmOpen} onOpenChange={setConfirmOpen}>
                <AlertDialogTrigger
                  render={<Button variant="ghost" size="sm" disabled={remove.isPending} />}
                >
                  <Trash data-icon="inline-start" />
                  {m.common_delete()}
                </AlertDialogTrigger>
                <AlertDialogContent>
                  <AlertDialogHeader>
                    <AlertDialogTitle>{m.users_delete_confirm({ name: user.name })}</AlertDialogTitle>
                    <AlertDialogDescription>{m.common_irreversible()}</AlertDialogDescription>
                  </AlertDialogHeader>
                  <AlertDialogFooter>
                    <AlertDialogCancel>{m.common_cancel()}</AlertDialogCancel>
                    <AlertDialogAction
                      variant="destructive"
                      data-testid="confirm-action"
                      onClick={() => {
                        setConfirmOpen(false)
                        remove.mutate()
                      }}
                    >
                      {m.common_delete()}
                    </AlertDialogAction>
                  </AlertDialogFooter>
                </AlertDialogContent>
              </AlertDialog>
            )}
          </ItemActions>
        )}
      </Item>

      <Sheet open={editOpen} onOpenChange={setEditOpen}>
        <SheetContent>
          <SheetHeader>
            <SheetTitle>{m.users_edit_title({ name: user.name })}</SheetTitle>
            <SheetDescription>{m.users_edit_description({ identity: identityLabel(user) })}</SheetDescription>
          </SheetHeader>
          <div className="flex flex-1 flex-col gap-6 overflow-y-auto px-4 pb-4">
            <form
              id={idPrefix}
              onSubmit={(e) => {
                e.preventDefault()
                if (quotaValid) save.mutate()
              }}
            >
              <FieldGroup>
                <FieldSet>
                  <FieldLegend variant="label">{m.users_section_quota()}</FieldLegend>
                  <div className="grid grid-cols-2 gap-3">
                    <Field>
                      <FieldLabel htmlFor={`${idPrefix}-gpus`}>{m.users_gpu_quota()}</FieldLabel>
                      <Input
                        id={`${idPrefix}-gpus`}
                        type="number"
                        min="0"
                        step="1"
                        aria-label={m.users_gpu_quota_for({ name: user.name })}
                        value={gpus}
                        onChange={(e) => setGpus(e.target.value)}
                      />
                    </Field>
                    <Field>
                      <FieldLabel htmlFor={`${idPrefix}-sandboxes`}>
                        {m.users_sandbox_quota()}
                      </FieldLabel>
                      <Input
                        id={`${idPrefix}-sandboxes`}
                        type="number"
                        min="0"
                        step="1"
                        aria-label={m.users_sandbox_quota_for({ name: user.name })}
                        value={sandboxes}
                        onChange={(e) => setSandboxes(e.target.value)}
                      />
                    </Field>
                  </div>
                </FieldSet>

                {save.error && (
                  <FieldError data-testid="user-row-error">
                    <Problem error={save.error} />
                  </FieldError>
                )}
                <Field orientation="horizontal">
                  <Button type="submit" disabled={save.isPending || !quotaValid}>
                    {m.users_save()}
                  </Button>
                </Field>
              </FieldGroup>
            </form>
          </div>
        </SheetContent>
      </Sheet>
    </>
  )
}
