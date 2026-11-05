import { useForm } from '@tanstack/react-form'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { createFileRoute } from '@tanstack/react-router'
import { Pencil, Plus, ShieldCheck, Trash } from 'lucide-react'
import { useState } from 'react'
import { z } from 'zod'

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
import { Checkbox } from '@/components/ui/checkbox'
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
import { NativeSelect, NativeSelectOption } from '@/components/ui/native-select'
import { Separator } from '@/components/ui/separator'
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
import { loginTypeLabel } from '@/lib/labels'
import { m } from '@/paraglide/messages'

export const Route = createFileRoute('/_authed/admin/users')({
  component: UsersPage,
})

const LOGIN_TYPES = ['password', 'oidc', 'none'] as const
type LoginType = (typeof LOGIN_TYPES)[number]

const createUserSchema = z
  .object({
    name: z.string().trim().min(1, { error: () => m.admin_user_name_required() }),
    email: z.string().trim(),
    login_type: z.enum(LOGIN_TYPES),
    password: z.string(),
    is_admin: z.boolean(),
  })
  .refine((value) => value.login_type !== 'password' || value.password.length >= 12, {
    error: () => m.admin_user_password_min(),
    path: ['password'],
  })

function UsersPage() {
  useLocale() // re-render on a language switch (see lib/i18n.tsx)
  const { user: me } = Route.useRouteContext()
  const queryClient = useQueryClient()
  const [createOpen, setCreateOpen] = useState(false)
  const [createError, setCreateError] = useState('')

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

  const create = useMutation({
    mutationFn: (body: {
      name: string
      email?: string
      login_type: LoginType
      password?: string
      is_admin: boolean
    }) => api<User>('POST', '/users', body),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ['users'] }),
  })

  const form = useForm({
    defaultValues: {
      name: '',
      email: '',
      login_type: 'password' as LoginType,
      password: '',
      is_admin: false,
    },
    onSubmit: async ({ value }) => {
      setCreateError('')
      const parsed = createUserSchema.safeParse(value)
      if (!parsed.success) {
        setCreateError(parsed.error.issues[0]?.message ?? m.common_invalid_input())
        return
      }
      const { name, email, login_type, password, is_admin } = parsed.data
      try {
        await create.mutateAsync({
          name,
          login_type,
          is_admin,
          ...(email ? { email } : {}),
          ...(login_type === 'password' ? { password } : {}),
        })
        form.reset()
        setCreateOpen(false)
      } catch {
        // The mutation error renders inside the drawer.
      }
    },
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
        actions={
          <Button
            onClick={() => {
              setCreateError('')
              setCreateOpen(true)
            }}
          >
            <Plus data-icon="inline-start" />
            {m.users_new_title()}
          </Button>
        }
      />

      <Sheet open={createOpen} onOpenChange={setCreateOpen}>
        <SheetContent>
          <SheetHeader>
            <SheetTitle>{m.users_new_title()}</SheetTitle>
            <SheetDescription>{m.users_new_description()}</SheetDescription>
          </SheetHeader>
          <form
            id="create-user-form"
            className="flex-1 overflow-y-auto px-4"
            onSubmit={(e) => {
              e.preventDefault()
              form.handleSubmit()
            }}
          >
            <FieldGroup>
              <form.Field name="name">
                {(field) => (
                  <Field>
                    <FieldLabel htmlFor="new-user-name">{m.common_name()}</FieldLabel>
                    <Input
                      id="new-user-name"
                      value={field.state.value}
                      onChange={(e) => field.handleChange(e.target.value)}
                    />
                  </Field>
                )}
              </form.Field>
              <form.Field name="email">
                {(field) => (
                  <Field>
                    <FieldLabel htmlFor="new-user-email">{m.users_email_optional()}</FieldLabel>
                    <Input
                      id="new-user-email"
                      type="email"
                      value={field.state.value}
                      onChange={(e) => field.handleChange(e.target.value)}
                    />
                  </Field>
                )}
              </form.Field>

              <form.Field name="login_type">
                {(field) => (
                  <Field>
                    <FieldLabel htmlFor="new-user-login-type">{m.users_login_type()}</FieldLabel>
                    <NativeSelect
                      id="new-user-login-type"
                      className="w-full"
                      value={field.state.value}
                      onChange={(e) => field.handleChange(e.target.value as LoginType)}
                    >
                      {LOGIN_TYPES.map((type) => (
                        <NativeSelectOption key={type} value={type}>
                          {loginTypeLabel(type)}
                        </NativeSelectOption>
                      ))}
                    </NativeSelect>
                  </Field>
                )}
              </form.Field>

              <form.Subscribe selector={(state) => state.values.login_type}>
                {(loginType) =>
                  loginType === 'password' ? (
                    <form.Field name="password">
                      {(field) => (
                        <Field>
                          <FieldLabel htmlFor="new-user-password">
                            {m.users_password_label()}
                          </FieldLabel>
                          <Input
                            id="new-user-password"
                            type="password"
                            autoComplete="new-password"
                            value={field.state.value}
                            onChange={(e) => field.handleChange(e.target.value)}
                          />
                        </Field>
                      )}
                    </form.Field>
                  ) : null
                }
              </form.Subscribe>

              <form.Field name="is_admin">
                {(field) => (
                  <Field orientation="horizontal">
                    <Checkbox
                      id="new-user-is-admin"
                      checked={field.state.value}
                      onCheckedChange={(checked) => field.handleChange(checked)}
                    />
                    <FieldLabel htmlFor="new-user-is-admin">{m.user_administrator()}</FieldLabel>
                  </Field>
                )}
              </form.Field>

              {(createError || create.error) && (
                <FieldError data-testid="create-user-error">
                  {createError || <Problem error={create.error} />}
                </FieldError>
              )}
            </FieldGroup>
          </form>
          <SheetFooter>
            <Button
              type="submit"
              form="create-user-form"
              disabled={form.state.isSubmitting || create.isPending}
            >
              <Plus data-icon="inline-start" />
              {m.users_create()}
            </Button>
            <Button type="button" variant="outline" onClick={() => setCreateOpen(false)}>
              {m.common_cancel()}
            </Button>
          </SheetFooter>
        </SheetContent>
      </Sheet>

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
  const [loginType, setLoginType] = useState<LoginType>(user.login_type)
  const [isAdmin, setIsAdmin] = useState(user.is_admin)
  const [gpus, setGpus] = useState(String(user.quota.gpus))
  const [sandboxes, setSandboxes] = useState(String(user.quota.sandboxes))
  const [password, setPassword] = useState('')
  const [confirmOpen, setConfirmOpen] = useState(false)

  const invalidate = () => queryClient.invalidateQueries({ queryKey: ['users'] })

  // One save for everything above the password: only what changed is sent.
  const save = useMutation({
    mutationFn: async () => {
      if (loginType !== user.login_type || isAdmin !== user.is_admin) {
        await api<User>('PATCH', `/users/${user.id}`, { login_type: loginType, is_admin: isAdmin })
      }
      if (Number(gpus) !== user.quota.gpus || Number(sandboxes) !== user.quota.sandboxes) {
        await api<User>('PUT', `/users/${user.id}/quota`, {
          gpus: Number(gpus),
          sandboxes: Number(sandboxes),
        })
      }
    },
    onSuccess: async () => {
      await invalidate()
      setEditOpen(false)
    },
    onError: invalidate,
  })
  const reset = useMutation({
    mutationFn: () => api<User>('PUT', `/users/${user.id}/password`, { password }),
    onSuccess: () => {
      setPassword('')
      invalidate()
    },
  })
  const remove = useMutation({
    mutationFn: () => api<User>('DELETE', `/users/${user.id}`),
    onSuccess: invalidate,
  })

  const openEditor = () => {
    // Start from the saved values, not from an abandoned earlier edit.
    setLoginType(user.login_type)
    setIsAdmin(user.is_admin)
    setGpus(String(user.quota.gpus))
    setSandboxes(String(user.quota.sandboxes))
    setPassword('')
    save.reset()
    reset.reset()
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
            {user.email ?? m.users_no_email()} · {loginTypeLabel(user.login_type)} ·{' '}
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
            <SheetDescription>{m.users_edit_description()}</SheetDescription>
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
                  <FieldLegend variant="label">{m.users_section_access()}</FieldLegend>
                  <Field>
                    <FieldLabel htmlFor={`${idPrefix}-login-type`}>{m.users_login_type()}</FieldLabel>
                    <NativeSelect
                      id={`${idPrefix}-login-type`}
                      className="w-full"
                      aria-label={m.users_login_type_for({ name: user.name })}
                      value={loginType}
                      onChange={(e) => setLoginType(e.target.value as LoginType)}
                    >
                      {LOGIN_TYPES.map((type) => (
                        <NativeSelectOption key={type} value={type}>
                          {loginTypeLabel(type)}
                        </NativeSelectOption>
                      ))}
                    </NativeSelect>
                  </Field>
                  <Field orientation="horizontal" data-disabled={isSelf || undefined}>
                    <Checkbox
                      id={`${idPrefix}-is-admin`}
                      aria-label={m.users_administrator_for({ name: user.name })}
                      checked={isAdmin}
                      disabled={isSelf}
                      onCheckedChange={(checked) => setIsAdmin(checked)}
                    />
                    <FieldLabel htmlFor={`${idPrefix}-is-admin`}>{m.users_admin_label()}</FieldLabel>
                  </Field>
                </FieldSet>

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

            {user.login_type === 'password' && (
              <>
                <Separator />
                <form
                  onSubmit={(e) => {
                    e.preventDefault()
                    if (password.length >= 12) reset.mutate()
                  }}
                >
                  <FieldSet>
                    <FieldLegend variant="label">{m.users_reset_password()}</FieldLegend>
                    <Field>
                      <FieldLabel htmlFor={`${idPrefix}-password`}>
                        {m.users_password_label()}
                      </FieldLabel>
                      <Input
                        id={`${idPrefix}-password`}
                        type="password"
                        autoComplete="new-password"
                        aria-label={m.users_new_password_for({ name: user.name })}
                        value={password}
                        onChange={(e) => {
                          setPassword(e.target.value)
                          reset.reset()
                        }}
                      />
                    </Field>
                    <Field orientation="horizontal">
                      <Button
                        type="submit"
                        variant="outline"
                        size="sm"
                        disabled={password.length < 12 || reset.isPending}
                      >
                        {m.users_reset_password()}
                      </Button>
                      {reset.isSuccess && (
                        <span className="text-xs text-success" role="status">
                          {m.users_password_reset_done()}
                        </span>
                      )}
                    </Field>
                    {reset.error && (
                      <FieldError>
                        <Problem error={reset.error} />
                      </FieldError>
                    )}
                  </FieldSet>
                </form>
              </>
            )}
          </div>
        </SheetContent>
      </Sheet>
    </>
  )
}
