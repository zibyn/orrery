import { useForm } from '@tanstack/react-form'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { createFileRoute } from '@tanstack/react-router'
import { Plus, ShieldCheck, Trash } from 'lucide-react'
import { useState } from 'react'
import { z } from 'zod'

import { api, type ListResponse, type QuotaUsage, type User } from '@/api'
import { PageHeader } from '@/components/page-header'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { messageOf, selectClass } from '@/lib/utils'

export const Route = createFileRoute('/_authed/admin/users')({
  component: UsersPage,
})

const LOGIN_TYPES = ['password', 'oidc', 'none'] as const
type LoginType = (typeof LOGIN_TYPES)[number]

const createUserSchema = z
  .object({
    name: z.string().trim().min(1, 'Enter a name'),
    email: z.string().trim(),
    login_type: z.enum(LOGIN_TYPES),
    password: z.string(),
    is_admin: z.boolean(),
  })
  .refine((value) => value.login_type !== 'password' || value.password.length >= 12, {
    message: 'Password must be at least 12 characters',
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
  const { user: me } = Route.useRouteContext()
  const queryClient = useQueryClient()

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
      const parsed = createUserSchema.safeParse(value)
      if (!parsed.success) return
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
      } catch {
        // The mutation error renders above the form.
      }
    },
  })

  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        title="Users"
        description="Create Users, change their login, reset passwords and set Quotas."
        meta={
          myQuota.data ? (
            <span data-testid="my-quota">
              Your usage: GPUs {myQuota.data.gpus.used}/{myQuota.data.gpus.limit} · Sandboxes{' '}
              {myQuota.data.sandboxes.used}/{myQuota.data.sandboxes.limit}
            </span>
          ) : undefined
        }
      />

      <Card>
        <CardHeader>
          <CardTitle>New User</CardTitle>
          <CardDescription>A User can sign in with a password, SSO, or not at all.</CardDescription>
        </CardHeader>
        <CardContent>
          <form
            className="flex flex-col gap-4"
            onSubmit={(e) => {
              e.preventDefault()
              form.handleSubmit()
            }}
          >
            <div className="grid gap-4 sm:grid-cols-2">
              <form.Field name="name">
                {(field) => (
                  <Field label="Name" htmlFor="new-user-name">
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
                  <Field label="Email (optional)" htmlFor="new-user-email">
                    <Input
                      id="new-user-email"
                      type="email"
                      value={field.state.value}
                      onChange={(e) => field.handleChange(e.target.value)}
                    />
                  </Field>
                )}
              </form.Field>
            </div>

            <div className="grid gap-4 sm:grid-cols-2">
              <form.Field name="login_type">
                {(field) => (
                  <Field label="Login type" htmlFor="new-user-login-type">
                    <select
                      id="new-user-login-type"
                      className={selectClass}
                      value={field.state.value}
                      onChange={(e) => field.handleChange(e.target.value as LoginType)}
                    >
                      {LOGIN_TYPES.map((type) => (
                        <option key={type} value={type}>
                          {type}
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
                        <Field
                          label="Password (at least 12 characters)"
                          htmlFor="new-user-password"
                        >
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
            </div>

            <form.Field name="is_admin">
              {(field) => (
                <label className="flex w-fit items-center gap-2 text-sm">
                  <input
                    type="checkbox"
                    className="size-4 accent-brand"
                    checked={field.state.value}
                    onChange={(e) => field.handleChange(e.target.checked)}
                  />
                  Administrator
                </label>
              )}
            </form.Field>

            {create.error && (
              <p className="text-sm text-destructive" role="alert" data-testid="create-user-error">
                {messageOf(create.error)}
              </p>
            )}

            <div>
              <Button type="submit" disabled={form.state.isSubmitting || create.isPending}>
                <Plus />
                Create User
              </Button>
            </div>
          </form>
        </CardContent>
      </Card>

      {users.isPending && <p className="py-8 text-sm text-muted-foreground">Loading users…</p>}
      {users.error && (
        <p className="text-sm text-destructive" role="alert">
          Could not load Users: {users.error.message}
        </p>
      )}

      {users.data && (
        <ul className="flex flex-col gap-3">
          {users.data.items.map((user) => (
            <UserRow key={user.id} user={user} isSelf={user.id === me.id} />
          ))}
        </ul>
      )}
    </div>
  )
}

function UserRow({ user, isSelf }: { user: User; isSelf: boolean }) {
  const queryClient = useQueryClient()
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

  const error = update.error ?? quota.error ?? reset.error ?? remove.error

  return (
    <li data-testid="user-row">
      <Card>
        <CardHeader className="flex-row items-start justify-between gap-3">
          <div className="flex min-w-0 items-center gap-3">
            <span
              aria-hidden="true"
              className="flex size-8 shrink-0 items-center justify-center rounded-full bg-muted text-xs font-medium"
            >
              {user.name.slice(0, 1).toUpperCase()}
            </span>
            <div className="min-w-0">
              <div className="flex flex-wrap items-center gap-2">
                <CardTitle className="truncate">{user.name}</CardTitle>
                {user.is_admin && (
                  <Badge variant="secondary">
                    <ShieldCheck />
                    admin
                  </Badge>
                )}
                {isSelf && <Badge variant="outline">you</Badge>}
                {user.deleted_at && <Badge variant="destructive">deleted</Badge>}
              </div>
              <CardDescription className="truncate">
                {user.email ?? 'no email'} · <span className="font-mono">{user.id}</span>
              </CardDescription>
            </div>
          </div>
          <Button
            variant="ghost"
            size="sm"
            disabled={isSelf || remove.isPending}
            onClick={() => {
              if (window.confirm(`Delete User ${user.name}?`)) remove.mutate()
            }}
          >
            <Trash />
            Delete
          </Button>
        </CardHeader>

        <CardContent className="flex flex-col gap-4 border-t border-border pt-4">
          <div className="flex flex-wrap items-end gap-3">
            <label className="flex flex-col gap-1.5 text-sm">
              <span className="text-xs text-muted-foreground">Login type</span>
              <select
                className={`${selectClass} w-32`}
                aria-label={`Login type for ${user.name}`}
                value={loginType}
                onChange={(e) => setLoginType(e.target.value as LoginType)}
              >
                {LOGIN_TYPES.map((type) => (
                  <option key={type} value={type}>
                    {type}
                  </option>
                ))}
              </select>
            </label>
            <label className="flex h-8 items-center gap-2 text-sm">
              <input
                type="checkbox"
                className="size-4 accent-brand"
                aria-label={`Administrator for ${user.name}`}
                checked={isAdmin}
                onChange={(e) => setIsAdmin(e.target.checked)}
              />
              Admin
            </label>
            <Button
              variant="outline"
              size="sm"
              disabled={update.isPending}
              onClick={() => update.mutate()}
            >
              Save login
            </Button>
          </div>

          <div className="flex flex-wrap items-end gap-3">
            <label className="flex flex-col gap-1.5 text-sm">
              <span className="text-xs text-muted-foreground">GPU quota</span>
              <Input
                type="number"
                min="0"
                className="w-28"
                aria-label={`GPU quota for ${user.name}`}
                value={gpus}
                onChange={(e) => setGpus(e.target.value)}
              />
            </label>
            <label className="flex flex-col gap-1.5 text-sm">
              <span className="text-xs text-muted-foreground">Sandbox quota</span>
              <Input
                type="number"
                min="0"
                className="w-28"
                aria-label={`Sandbox quota for ${user.name}`}
                value={sandboxes}
                onChange={(e) => setSandboxes(e.target.value)}
              />
            </label>
            <Button
              variant="outline"
              size="sm"
              disabled={quota.isPending}
              onClick={() => quota.mutate()}
            >
              Set quota
            </Button>
            <span className="pb-1.5 text-xs text-muted-foreground">
              Limits {user.quota.gpus} GPUs / {user.quota.sandboxes} Sandboxes
            </span>
          </div>

          {user.login_type === 'password' && (
            <div className="flex flex-wrap items-end gap-3">
              <label className="flex flex-col gap-1.5 text-sm">
                <span className="text-xs text-muted-foreground">New password</span>
                <Input
                  type="password"
                  autoComplete="new-password"
                  aria-label={`New password for ${user.name}`}
                  value={password}
                  onChange={(e) => setPassword(e.target.value)}
                />
              </label>
              <Button
                variant="outline"
                size="sm"
                disabled={password.length < 12 || reset.isPending}
                onClick={() => reset.mutate()}
              >
                Reset password
              </Button>
            </div>
          )}

          {error && (
            <p className="text-sm text-destructive" role="alert" data-testid="user-row-error">
              {messageOf(error)}
            </p>
          )}
        </CardContent>
      </Card>
    </li>
  )
}
