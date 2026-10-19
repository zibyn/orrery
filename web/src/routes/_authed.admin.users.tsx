import { useForm } from '@tanstack/react-form'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { createFileRoute } from '@tanstack/react-router'
import { Pencil, Plus, ShieldCheck, Trash } from 'lucide-react'
import { useState } from 'react'
import { z } from 'zod'

import { api, type ListResponse, type QuotaUsage, type User } from '@/api'
import { Breadcrumbs } from '@/components/breadcrumbs'
import { ListCard, ListRow } from '@/components/list'
import { PageHeader } from '@/components/page-header'
import { Problem } from '@/components/problem'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Drawer } from '@/components/ui/drawer'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { useLocale } from '@/lib/i18n'
import { loginTypeLabel } from '@/lib/labels'
import { selectClass } from '@/lib/utils'
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

function Field({
  label,
  htmlFor,
  children,
}: {
  label: string
  htmlFor: string
  children: React.ReactNode
}) {
  return (
    <div className="flex flex-col gap-1.5">
      <Label htmlFor={htmlFor}>{label}</Label>
      {children}
    </div>
  )
}

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
          <Breadcrumbs items={[{ label: m.nav_group_admin() }, { label: m.users_title() }]} />
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
            <Plus />
            {m.users_new_title()}
          </Button>
        }
      />

      <Drawer
        open={createOpen}
        onOpenChange={setCreateOpen}
        title={m.users_new_title()}
        description={m.users_new_description()}
        footer={
          <>
            <Button type="button" variant="ghost" onClick={() => setCreateOpen(false)}>
              {m.common_cancel()}
            </Button>
            <Button
              type="submit"
              form="create-user-form"
              disabled={form.state.isSubmitting || create.isPending}
            >
              <Plus />
              {m.users_create()}
            </Button>
          </>
        }
      >
        <form
          id="create-user-form"
          className="flex flex-col gap-4"
          onSubmit={(e) => {
            e.preventDefault()
            form.handleSubmit()
          }}
        >
          <form.Field name="name">
            {(field) => (
              <Field label={m.common_name()} htmlFor="new-user-name">
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
              <Field label={m.users_email_optional()} htmlFor="new-user-email">
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
              <Field label={m.users_login_type()} htmlFor="new-user-login-type">
                <select
                  id="new-user-login-type"
                  className={selectClass}
                  value={field.state.value}
                  onChange={(e) => field.handleChange(e.target.value as LoginType)}
                >
                  {LOGIN_TYPES.map((type) => (
                    <option key={type} value={type}>
                      {loginTypeLabel(type)}
                    </option>
                  ))}
                </select>
              </Field>
            )}
          </form.Field>

          <form.Subscribe selector={(state) => state.values.login_type}>
            {(loginType) =>
              loginType === 'password' ? (
                <form.Field name="password">
                  {(field) => (
                    <Field label={m.users_password_label()} htmlFor="new-user-password">
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
              <label className="flex w-fit items-center gap-2 text-sm">
                <input
                  type="checkbox"
                  className="size-4 accent-brand"
                  checked={field.state.value}
                  onChange={(e) => field.handleChange(e.target.checked)}
                />
                {m.user_administrator()}
              </label>
            )}
          </form.Field>

          {(createError || create.error) && (
            <p className="text-sm text-destructive" role="alert" data-testid="create-user-error">
              {createError || <Problem error={create.error} />}
            </p>
          )}
        </form>
      </Drawer>

      {users.isPending && (
        <p className="py-8 text-sm text-muted-foreground">{m.users_loading()}</p>
      )}
      {users.error && (
        <p className="text-sm text-destructive" role="alert">
          {m.users_load_error()}: <Problem error={users.error} />
        </p>
      )}

      {users.data && users.data.items.length > 0 && (
        <ListCard>
          {users.data.items.map((user) => (
            <UserRow key={user.id} user={user} isSelf={user.id === me.id} />
          ))}
        </ListCard>
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

  const invalidate = () => queryClient.invalidateQueries({ queryKey: ['users'] })

  const update = useMutation({
    mutationFn: () =>
      api<User>('PATCH', `/users/${user.id}`, { login_type: loginType, is_admin: isAdmin }),
    onSuccess: invalidate,
  })
  const quota = useMutation({
    mutationFn: () =>
      api<User>('PUT', `/users/${user.id}/quota`, {
        gpus: Number(gpus),
        sandboxes: Number(sandboxes),
      }),
    onSuccess: invalidate,
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

  const error = update.error ?? quota.error ?? reset.error

  return (
    <>
      <ListRow
        data-testid="user-row"
        icon={<span aria-hidden="true">{user.name.slice(0, 1).toUpperCase()}</span>}
        title={
          <>
            <span className="truncate text-sm font-medium">{user.name}</span>
            {user.is_admin && (
              <Badge variant="secondary">
                <ShieldCheck />
                {m.users_badge_admin()}
              </Badge>
            )}
            {isSelf && <Badge variant="outline">{m.users_badge_you()}</Badge>}
            {user.deleted_at && <Badge variant="destructive">{m.users_badge_deleted()}</Badge>}
          </>
        }
        description={
          <>
            {user.email ?? m.users_no_email()} · <span className="font-mono">{user.id}</span> ·{' '}
            {m.users_limits({ gpus: user.quota.gpus, sandboxes: user.quota.sandboxes })}
            {remove.error && (
              <span className="block text-destructive" role="alert">
                <Problem error={remove.error} />
              </span>
            )}
          </>
        }
        trailing={
          <>
            <Button variant="outline" size="sm" onClick={() => setEditOpen(true)}>
              <Pencil />
              {m.common_edit()}
            </Button>
            <Button
              variant="ghost"
              size="sm"
              disabled={isSelf || remove.isPending}
              onClick={() => {
                if (window.confirm(m.users_delete_confirm({ name: user.name }))) remove.mutate()
              }}
            >
              <Trash />
              {m.common_delete()}
            </Button>
          </>
        }
      />

      <Drawer
        open={editOpen}
        onOpenChange={setEditOpen}
        title={m.users_edit_title({ name: user.name })}
        description={m.users_edit_description()}
      >
        <div className="flex flex-col gap-5">
          <div className="flex flex-col gap-3">
            <label className="flex flex-col gap-1.5 text-sm">
              <span className="text-xs text-muted-foreground">{m.users_login_type()}</span>
              <select
                className={selectClass}
                aria-label={m.users_login_type_for({ name: user.name })}
                value={loginType}
                onChange={(e) => setLoginType(e.target.value as LoginType)}
              >
                {LOGIN_TYPES.map((type) => (
                  <option key={type} value={type}>
                    {loginTypeLabel(type)}
                  </option>
                ))}
              </select>
            </label>
            <label className="flex items-center gap-2 text-sm">
              <input
                type="checkbox"
                className="size-4 accent-brand"
                aria-label={m.users_administrator_for({ name: user.name })}
                checked={isAdmin}
                onChange={(e) => setIsAdmin(e.target.checked)}
              />
              {m.users_admin_label()}
            </label>
            <Button
              className="self-start"
              variant="outline"
              size="sm"
              disabled={update.isPending}
              onClick={() => update.mutate()}
            >
              {m.users_save_login()}
            </Button>
          </div>

          <div className="flex flex-col gap-3 border-t border-border pt-5">
            <div className="grid grid-cols-2 gap-3">
              <label className="flex flex-col gap-1.5 text-sm">
                <span className="text-xs text-muted-foreground">{m.users_gpu_quota()}</span>
                <Input
                  type="number"
                  min="0"
                  aria-label={m.users_gpu_quota_for({ name: user.name })}
                  value={gpus}
                  onChange={(e) => setGpus(e.target.value)}
                />
              </label>
              <label className="flex flex-col gap-1.5 text-sm">
                <span className="text-xs text-muted-foreground">{m.users_sandbox_quota()}</span>
                <Input
                  type="number"
                  min="0"
                  aria-label={m.users_sandbox_quota_for({ name: user.name })}
                  value={sandboxes}
                  onChange={(e) => setSandboxes(e.target.value)}
                />
              </label>
            </div>
            <div className="flex flex-wrap items-center gap-3">
              <Button
                variant="outline"
                size="sm"
                disabled={
                  quota.isPending ||
                  !Number.isFinite(Number(gpus)) ||
                  !Number.isFinite(Number(sandboxes))
                }
                onClick={() => quota.mutate()}
              >
                {m.users_set_quota()}
              </Button>
              <span className="text-xs text-muted-foreground">
                {m.users_limits({ gpus: user.quota.gpus, sandboxes: user.quota.sandboxes })}
              </span>
            </div>
          </div>

          {user.login_type === 'password' && (
            <div className="flex flex-col gap-3 border-t border-border pt-5">
              <label className="flex flex-col gap-1.5 text-sm">
                <span className="text-xs text-muted-foreground">{m.users_new_password()}</span>
                <Input
                  type="password"
                  autoComplete="new-password"
                  aria-label={m.users_new_password_for({ name: user.name })}
                  value={password}
                  onChange={(e) => setPassword(e.target.value)}
                />
              </label>
              <Button
                className="self-start"
                variant="outline"
                size="sm"
                disabled={password.length < 12 || reset.isPending}
                onClick={() => reset.mutate()}
              >
                {m.users_reset_password()}
              </Button>
            </div>
          )}

          {error && (
            <p className="text-sm text-destructive" role="alert" data-testid="user-row-error">
              <Problem error={error} />
            </p>
          )}
        </div>
      </Drawer>
    </>
  )
}
