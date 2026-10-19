import { useForm } from '@tanstack/react-form'
import { useQuery } from '@tanstack/react-query'
import { createFileRoute, useNavigate } from '@tanstack/react-router'
import { useState, type ReactNode } from 'react'

import {
  api,
  type CapacityNode,
  type CreateSandboxRequest,
  type ListResponse,
  type Sandbox,
  type Snapshot,
  type Workspace,
} from '@/api'
import { Breadcrumbs } from '@/components/breadcrumbs'
import { PageHeader } from '@/components/page-header'
import { Problem } from '@/components/problem'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardFooter, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { useLocale } from '@/lib/i18n'
import { messageOf } from '@/lib/labels'
import { buildCreateSandboxBody, createSandboxSchema } from '@/lib/sandbox-form'
import { selectClass } from '@/lib/utils'
import { m } from '@/paraglide/messages'

export const Route = createFileRoute('/_authed/sandboxes/new')({
  component: NewSandboxPage,
})

function Field({
  label,
  htmlFor,
  hint,
  children,
}: {
  label: string
  htmlFor: string
  hint?: string
  children: ReactNode
}) {
  return (
    <div className="flex flex-col gap-1.5">
      <Label htmlFor={htmlFor}>{label}</Label>
      {children}
      {hint && <p className="text-xs text-muted-foreground">{hint}</p>}
    </div>
  )
}

function NewSandboxPage() {
  useLocale() // re-render on a language switch (see lib/i18n.tsx)
  const navigate = useNavigate()
  const [error, setError] = useState('')

  const snapshots = useQuery({
    queryKey: ['snapshots'],
    queryFn: () => api<ListResponse<Snapshot>>('GET', '/snapshots'),
  })
  const capacity = useQuery({
    queryKey: ['capacity'],
    queryFn: () => api<ListResponse<CapacityNode>>('GET', '/capacity'),
  })
  const workspaces = useQuery({
    queryKey: ['workspaces'],
    queryFn: () => api<ListResponse<Workspace>>('GET', '/workspaces'),
  })

  const gpuModels = Array.from(
    new Set(
      (capacity.data?.items ?? []).flatMap((node) =>
        node.gpus.filter((gpu) => gpu.total > 0).map((gpu) => gpu.model),
      ),
    ),
  )
  const attachable = (workspaces.data?.items ?? []).filter(
    (workspace) =>
      workspace.kind === 'persistent' &&
      (workspace.state === 'ready' || workspace.state === 'provisioning'),
  )

  const form = useForm({
    defaultValues: {
      name: '',
      snapshot: '',
      cpu: '1',
      memoryMiB: '512',
      gpuCount: '0',
      gpuModel: '',
      workspaceId: '',
      workspaceMode: 'rw' as 'rw' | 'ro',
      idleStop: '900',
    },
    onSubmit: async ({ value }) => {
      setError('')
      const parsed = createSandboxSchema.safeParse(value)
      if (!parsed.success) {
        setError(parsed.error.issues[0]?.message ?? m.common_invalid_input())
        return
      }
      try {
        const created = await api<Sandbox>(
          'POST',
          '/sandboxes',
          buildCreateSandboxBody(parsed.data) satisfies CreateSandboxRequest,
        )
        navigate({ to: '/sandboxes/$id', params: { id: created.id } })
      } catch (err) {
        setError(messageOf(err))
      }
    },
  })

  const loadError = snapshots.error ?? capacity.error ?? workspaces.error

  return (
    <div className="flex max-w-3xl flex-col gap-6">
      <PageHeader
        above={
          <Breadcrumbs
            items={[
              { label: m.nav_sandboxes(), to: '/sandboxes' },
              { label: m.new_title() },
            ]}
          />
        }
        title={m.new_title()}
        description={m.new_description()}
      />

      <form
        className="flex flex-col gap-5"
        onSubmit={(e) => {
          e.preventDefault()
          form.handleSubmit()
        }}
      >
        <Card>
          <CardHeader>
            <CardTitle>{m.new_general()}</CardTitle>
            <CardDescription>{m.new_general_description()}</CardDescription>
          </CardHeader>
          <CardContent className="flex flex-col gap-4">
            <form.Field name="name">
              {(field) => (
                <Field label={m.common_name()} htmlFor={field.name}>
                  <Input
                    id={field.name}
                    value={field.state.value}
                    onChange={(e) => field.handleChange(e.target.value)}
                    placeholder="my-sandbox"
                    autoFocus
                  />
                </Field>
              )}
            </form.Field>

            <form.Field name="snapshot">
              {(field) => (
                <Field label="Snapshot" htmlFor={field.name}>
                  <select
                    id={field.name}
                    className={selectClass}
                    value={field.state.value}
                    onChange={(e) => field.handleChange(e.target.value)}
                  >
                    <option value="">{m.new_deployment_default()}</option>
                    {snapshots.data?.items.map((snapshot) => (
                      <option key={snapshot.id} value={snapshot.name}>
                        {snapshot.name}
                        {snapshot.owner_id === null ? m.new_platform_suffix() : ''}
                      </option>
                    ))}
                  </select>
                </Field>
              )}
            </form.Field>
          </CardContent>
        </Card>

        <Card>
          <CardHeader>
            <CardTitle>{m.new_resources()}</CardTitle>
            <CardDescription>{m.new_resources_description()}</CardDescription>
          </CardHeader>
          <CardContent className="flex flex-col gap-4">
            <div className="grid grid-cols-2 gap-4">
              <form.Field name="cpu">
                {(field) => (
                  <Field label="CPU" htmlFor={field.name}>
                    <Input
                      id={field.name}
                      type="number"
                      min="0.1"
                      step="0.1"
                      value={field.state.value}
                      onChange={(e) => field.handleChange(e.target.value)}
                    />
                  </Field>
                )}
              </form.Field>
              <form.Field name="memoryMiB">
                {(field) => (
                  <Field label={m.new_memory_mib()} htmlFor={field.name}>
                    <Input
                      id={field.name}
                      type="number"
                      min="1"
                      step="1"
                      value={field.state.value}
                      onChange={(e) => field.handleChange(e.target.value)}
                    />
                  </Field>
                )}
              </form.Field>
            </div>

            <div className="grid grid-cols-2 gap-4">
              <form.Field name="gpuCount">
                {(field) => (
                  <Field label={m.new_gpus()} htmlFor={field.name}>
                    <Input
                      id={field.name}
                      type="number"
                      min="0"
                      step="1"
                      value={field.state.value}
                      onChange={(e) => field.handleChange(e.target.value)}
                    />
                  </Field>
                )}
              </form.Field>
              <form.Field name="gpuModel">
                {(field) => (
                  <Field label={m.new_gpu_model()} htmlFor={field.name}>
                    <select
                      id={field.name}
                      className={selectClass}
                      value={field.state.value}
                      onChange={(e) => field.handleChange(e.target.value)}
                    >
                      <option value="">{m.new_any_model()}</option>
                      {gpuModels.map((model) => (
                        <option key={model} value={model}>
                          {model}
                        </option>
                      ))}
                    </select>
                  </Field>
                )}
              </form.Field>
            </div>
          </CardContent>
        </Card>

        <Card>
          <CardHeader>
            <CardTitle>Workspace</CardTitle>
            <CardDescription>{m.new_workspace_description()}</CardDescription>
          </CardHeader>
          <CardContent className="flex flex-col gap-4">
            <form.Field name="workspaceId">
              {(field) => (
                <Field label="Workspace" htmlFor={field.name}>
                  <select
                    id={field.name}
                    className={selectClass}
                    value={field.state.value}
                    onChange={(e) => field.handleChange(e.target.value)}
                  >
                    <option value="">{m.new_ephemeral()}</option>
                    {attachable.map((workspace) => (
                      <option key={workspace.id} value={workspace.id}>
                        {workspace.id}
                      </option>
                    ))}
                  </select>
                </Field>
              )}
            </form.Field>

            <form.Subscribe selector={(state) => state.values.workspaceId}>
              {(workspaceId) =>
                workspaceId ? (
                  <form.Field name="workspaceMode">
                    {(field) => (
                      <Field label={m.new_workspace_mode()} htmlFor={field.name}>
                        <select
                          id={field.name}
                          className={selectClass}
                          value={field.state.value}
                          onChange={(e) => field.handleChange(e.target.value as 'rw' | 'ro')}
                        >
                          <option value="rw">{m.new_read_write()}</option>
                          <option value="ro">{m.new_read_only()}</option>
                        </select>
                      </Field>
                    )}
                  </form.Field>
                ) : null
              }
            </form.Subscribe>

            <form.Field name="idleStop">
              {(field) => (
                <Field
                  label={m.new_idle_stop()}
                  htmlFor={field.name}
                  hint={m.new_idle_stop_hint()}
                >
                  <Input
                    id={field.name}
                    type="number"
                    min="0"
                    step="1"
                    value={field.state.value}
                    onChange={(e) => field.handleChange(e.target.value)}
                  />
                </Field>
              )}
            </form.Field>
          </CardContent>
          <CardFooter className="justify-end">
            <Button type="button" variant="ghost" onClick={() => navigate({ to: '/sandboxes' })}>
              {m.common_cancel()}
            </Button>
            <Button type="submit" disabled={form.state.isSubmitting}>
              {m.new_create()}
            </Button>
          </CardFooter>
        </Card>

        {(error || loadError) && (
          <p className="text-sm whitespace-pre-line text-destructive" role="alert">
            {error || <Problem error={loadError} />}
          </p>
        )}
      </form>
    </div>
  )
}
