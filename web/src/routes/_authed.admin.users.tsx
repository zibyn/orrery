import { useForm } from '@tanstack/react-form'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { createFileRoute } from '@tanstack/react-router'
import { useState } from 'react'
import { z } from 'zod'

import { api, type ListResponse, type QuotaUsage, type User } from '@/api'
import { Button } from '@/components/ui/button'
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
    <div className="flex flex-col gap-8">
      <div className="flex flex-wrap items-end justify-between gap-4">
        <div>
          <h1 className="text-2xl font-semibold">Users</h1>
          <p className="text-sm text-muted-foreground">
            Create Users, change their login, reset passwords and set Quotas.
          </p>
        </div>
        {myQuota.data && (
          <p className="text-sm text-muted-foreground" data-testid="my-quota">
            Your usage: GPUs {myQuota.data.gpus.used}/{myQuota.data.gpus.limit} · Sandboxes{' '}
            {myQuota.data.sandboxes.used}/{myQuota.data.sandboxes.limit}
          </p>
        )}
      </div>

      <form
        className="flex flex-col gap-4 rounded-md border border-border p-4"
        onSubmit={(e) => {
          e.preventDefault()
          form.handleSubmit()
        }}
      >
        <h2 className="font-medium">New User</h2>
        <div className="grid grid-cols-2 gap-4">
          <form.Field name="name">
            {(field) => (
              <div className="flex flex-col gap-2">
                <Label htmlFor="new-user-name">Name</Label>
                <Input
                  id="new-user-name"
                  value={field.state.value}
                  onChange={(e) => field.handleChange(e.target.value)}
                />
              </div>
            )}
          </form.Field>
          <form.Field name="email">
            {(field) => (
              <div className="flex flex-col gap-2">
                <Label htmlFor="new-user-email">Email (optional)</Label>
                <Input
                  id="new-user-email"
                  type="email"
                  value={field.state.value}
                  onChange={(e) => field.handleChange(e.target.value)}
                />
              </div>
            )}
          </form.Field>
        </div>
        <div className="grid grid-cols-2 gap-4">
          <form.Field name="login_type">
            {(field) => (
              <div className="flex flex-col gap-2">
                <Label htmlFor="new-user-login-type">Login type</Label>
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
              </div>
            )}
          </form.Field>
          <form.Subscribe selector={(state) => state.values.login_type}>
            {(loginType) =>
              loginType === 'password' ? (
                <form.Field name="password">
                  {(field) => (
                    <div className="flex flex-col gap-2">
                      <Label htmlFor="new-user-password">Password (at least 12 characters)</Label>
                      <Input
                        id="new-user-password"
                        type="password"
                        autoComplete="new-password"
                        value={field.state.value}
                        onChange={(e) => field.handleChange(e.target.value)}
                      />
                    </div>
                  )}
                </form.Field>
              ) : null
            }
          </form.Subscribe>
        </div>
        <form.Field name="is_admin">
          {(field) => (
            <label className="flex items-center gap-2 text-sm">
              <input
                type="checkbox"
                checked={field.state.value}
                onChange={(e) => field.handleChange(e.target.checked)}
              />
              Administrator
            </label>
          )}
        </form.Field>
        {create.error && (
          <p className="text-sm text-destructive" data-testid="create-user-error">
            {messageOf(create.error)}
          </p>
        )}
        <div>
          <Button type="submit" disabled={form.state.isSubmitting || create.isPending}>
            Create User
          </Button>
        </div>
      </form>

      {users.isPending && <p className="text-sm text-muted-foreground">Loading…</p>}
      {users.error && (
        <p className="text-sm text-destructive">Could not load Users: {users.error.message}</p>
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
    <li
      className="flex flex-col gap-4 rounded-md border border-border p-4"
      data-testid="user-row"
    >
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <p className="font-medium">{user.name}</p>
          <p className="text-xs text-muted-foreground">
            {user.email ?? 'no email'} · id {user.id}
            {user.deleted_at ? ' · deleted' : ''}
          </p>
        </div>
        <Button
          variant="outline"
          size="sm"
          disabled={isSelf || remove.isPending}
          onClick={() => {
            if (window.confirm(`Delete User ${user.name}?`)) remove.mutate()
          }}
        >
          Delete
        </Button>
      </div>

      <div className="flex flex-wrap items-end gap-3">
        <label className="flex flex-col gap-1 text-sm">
          <span className="text-muted-foreground">Login type</span>
          <select
            className={selectClass}
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
        <label className="flex h-9 items-center gap-2 text-sm">
          <input
            type="checkbox"
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
        <label className="flex flex-col gap-1 text-sm">
          <span className="text-muted-foreground">GPU quota</span>
          <Input
            type="number"
            min="0"
            className="w-28"
            aria-label={`GPU quota for ${user.name}`}
            value={gpus}
            onChange={(e) => setGpus(e.target.value)}
          />
        </label>
        <label className="flex flex-col gap-1 text-sm">
          <span className="text-muted-foreground">Sandbox quota</span>
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
        <span className="text-xs text-muted-foreground">
          Limits {user.quota.gpus} GPUs / {user.quota.sandboxes} Sandboxes
        </span>
      </div>

      {user.login_type === 'password' && (
        <div className="flex flex-wrap items-end gap-3">
          <label className="flex flex-col gap-1 text-sm">
            <span className="text-muted-foreground">New password</span>
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
        <p className="text-sm text-destructive" data-testid="user-row-error">
          {messageOf(error)}
        </p>
      )}
    </li>
  )
}
