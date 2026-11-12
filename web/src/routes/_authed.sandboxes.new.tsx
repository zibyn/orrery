import { useForm } from '@tanstack/react-form'
import { useQuery } from '@tanstack/react-query'
import { createFileRoute, Link, useNavigate } from '@tanstack/react-router'
import { useState } from 'react'

import {
  api,
  type CapacityNode,
  type CreateSandboxRequest,
  listAll,
  type ListResponse,
  type Sandbox,
  type Snapshot,
  type Workspace,
} from '@/api'
import { PageHeader } from '@/components/page-header'
import { notifyError } from '@/lib/notify'
import {
  Breadcrumb,
  BreadcrumbItem,
  BreadcrumbLink,
  BreadcrumbList,
  BreadcrumbPage,
  BreadcrumbSeparator,
} from '@/components/ui/breadcrumb'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardFooter, CardHeader, CardTitle } from '@/components/ui/card'
import { Field, FieldDescription, FieldError, FieldGroup, FieldLabel } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { NativeSelect, NativeSelectOption } from '@/components/ui/native-select'
import { useLocale } from '@/lib/i18n'
import { buildCreateSandboxBody, createSandboxSchema } from '@/lib/sandbox-form'
import { m } from '@/paraglide/messages'

export const Route = createFileRoute('/_authed/sandboxes/new')({
  // Snapshot and Workspace pages link here with their own value prefilled.
  validateSearch: (search: Record<string, unknown>): { snapshot?: string; workspace?: string } => ({
    snapshot: typeof search.snapshot === 'string' ? search.snapshot : undefined,
    workspace: typeof search.workspace === 'string' ? search.workspace : undefined,
  }),
  component: NewSandboxPage,
})

function NewSandboxPage() {
  useLocale() // re-render on a language switch (see lib/i18n.tsx)
  const navigate = useNavigate()
  const prefill = Route.useSearch()
  const [error, setError] = useState<string>()

  const snapshots = useQuery({
    queryKey: ['snapshots'],
    queryFn: () => listAll<Snapshot>('/snapshots'),
  })
  const capacity = useQuery({
    queryKey: ['capacity'],
    queryFn: () => api<ListResponse<CapacityNode>>('GET', '/capacity'),
  })
  const workspaces = useQuery({
    queryKey: ['workspaces'],
    queryFn: () => listAll<Workspace>('/workspaces'),
  })

  const gpuModels = Array.from(
    new Set(
      (capacity.data?.items ?? []).flatMap((node) =>
        node.gpus.filter((gpu) => gpu.total > 0).map((gpu) => gpu.model),
      ),
    ),
  )
  const attachable = (workspaces.data ?? []).filter(
    (workspace) =>
      workspace.kind === 'persistent' &&
      (workspace.state === 'ready' || workspace.state === 'provisioning'),
  )

  const form = useForm({
    defaultValues: {
      name: '',
      snapshot: prefill.snapshot ?? '',
      cpu: '1',
      memoryMiB: '512',
      gpuCount: '0',
      gpuModel: '',
      workspaceId: prefill.workspace ?? '',
      workspaceMode: 'rw' as 'rw' | 'ro',
      idleStop: '900',
    },
    onSubmit: async ({ value }) => {
      setError(undefined)
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
        notifyError(err)
      }
    },
  })

  return (
    <div className="flex max-w-3xl flex-col gap-6">
      <PageHeader
        above={
          <Breadcrumb aria-label={m.common_breadcrumb()}>
            <BreadcrumbList>
              <BreadcrumbItem>
                <BreadcrumbLink render={<Link to="/sandboxes" />}>{m.nav_sandboxes()}</BreadcrumbLink>
              </BreadcrumbItem>
              <BreadcrumbSeparator />
              <BreadcrumbItem>
                <BreadcrumbPage>{m.new_title()}</BreadcrumbPage>
              </BreadcrumbItem>
            </BreadcrumbList>
          </Breadcrumb>
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
          <CardContent>
            <FieldGroup>
              <form.Field name="name">
                {(field) => (
                  <Field>
                    <FieldLabel htmlFor={field.name}>{m.common_name()}</FieldLabel>
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
                  <Field>
                    <FieldLabel htmlFor={field.name}>{m.label_snapshot()}</FieldLabel>
                    <NativeSelect
                      id={field.name}
                      className="w-full"
                      value={field.state.value}
                      onChange={(e) => field.handleChange(e.target.value)}
                    >
                      <NativeSelectOption value="">{m.new_deployment_default()}</NativeSelectOption>
                      {snapshots.data?.map((snapshot) => (
                        <NativeSelectOption key={snapshot.id} value={snapshot.name}>
                          {snapshot.name}
                          {snapshot.owner_id === null ? m.new_platform_suffix() : ''}
                        </NativeSelectOption>
                      ))}
                    </NativeSelect>
                  </Field>
                )}
              </form.Field>

              <form.Field name="idleStop">
                {(field) => (
                  <Field>
                    <FieldLabel htmlFor={field.name}>{m.new_idle_stop()}</FieldLabel>
                    <Input
                      id={field.name}
                      type="number"
                      min="0"
                      step="1"
                      value={field.state.value}
                      onChange={(e) => field.handleChange(e.target.value)}
                    />
                    <FieldDescription>{m.new_idle_stop_hint()}</FieldDescription>
                  </Field>
                )}
              </form.Field>
            </FieldGroup>
          </CardContent>
        </Card>

        <Card>
          <CardHeader>
            <CardTitle>{m.new_resources()}</CardTitle>
            <CardDescription>{m.new_resources_description()}</CardDescription>
          </CardHeader>
          <CardContent>
            <FieldGroup>
              <div className="grid grid-cols-2 gap-4">
                <form.Field name="cpu">
                  {(field) => (
                    <Field>
                      <FieldLabel htmlFor={field.name}>CPU</FieldLabel>
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
                    <Field>
                      <FieldLabel htmlFor={field.name}>{m.new_memory_mib()}</FieldLabel>
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
                    <Field>
                      <FieldLabel htmlFor={field.name}>{m.new_gpus()}</FieldLabel>
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
                    <Field>
                      <FieldLabel htmlFor={field.name}>{m.new_gpu_model()}</FieldLabel>
                      <NativeSelect
                        id={field.name}
                        className="w-full"
                        value={field.state.value}
                        onChange={(e) => field.handleChange(e.target.value)}
                      >
                        <NativeSelectOption value="">{m.new_any_model()}</NativeSelectOption>
                        {gpuModels.map((model) => (
                          <NativeSelectOption key={model} value={model}>
                            {model}
                          </NativeSelectOption>
                        ))}
                      </NativeSelect>
                    </Field>
                  )}
                </form.Field>
              </div>
            </FieldGroup>
          </CardContent>
        </Card>

        <Card>
          <CardHeader>
            <CardTitle>{m.label_workspace()}</CardTitle>
            <CardDescription>{m.new_workspace_description()}</CardDescription>
          </CardHeader>
          <CardContent>
            <FieldGroup>
              <form.Field name="workspaceId">
                {(field) => (
                  <Field>
                    <FieldLabel htmlFor={field.name}>{m.label_workspace()}</FieldLabel>
                    <NativeSelect
                      id={field.name}
                      className="w-full"
                      value={field.state.value}
                      onChange={(e) => field.handleChange(e.target.value)}
                    >
                      <NativeSelectOption value="">{m.new_ephemeral()}</NativeSelectOption>
                      {attachable.map((workspace) => (
                        <NativeSelectOption key={workspace.id} value={workspace.id}>
                          {workspace.id}
                        </NativeSelectOption>
                      ))}
                    </NativeSelect>
                  </Field>
                )}
              </form.Field>

              <form.Subscribe selector={(state) => state.values.workspaceId}>
                {(workspaceId) =>
                  workspaceId ? (
                    <form.Field name="workspaceMode">
                      {(field) => (
                        <Field>
                          <FieldLabel htmlFor={field.name}>{m.new_workspace_mode()}</FieldLabel>
                          <NativeSelect
                            id={field.name}
                            className="w-full"
                            value={field.state.value}
                            onChange={(e) => field.handleChange(e.target.value as 'rw' | 'ro')}
                          >
                            <NativeSelectOption value="rw">{m.new_read_write()}</NativeSelectOption>
                            <NativeSelectOption value="ro">{m.new_read_only()}</NativeSelectOption>
                          </NativeSelect>
                        </Field>
                      )}
                    </form.Field>
                  ) : null
                }
              </form.Subscribe>

            </FieldGroup>
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

        {error && (
          <FieldError>
            {error}
          </FieldError>
        )}
      </form>
    </div>
  )
}
