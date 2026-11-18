import { useForm } from '@tanstack/react-form'
import { useQuery } from '@tanstack/react-query'
import { createFileRoute, Link, useNavigate } from '@tanstack/react-router'
import { Plus, X } from 'lucide-react'
import { useState } from 'react'

import {
  api,
  type CapacityNode,
  type CreateSandboxRequest,
  listAll,
  type ListResponse,
  type Sandbox,
  type Snapshot,
  type Volume,
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
import { buildCreateSandboxBody, createSandboxSchema, volumeMountProblem } from '@/lib/sandbox-form'
import { m } from '@/paraglide/messages'

export const Route = createFileRoute('/_authed/sandboxes/new')({
  // Snapshot and Volume pages link here with their own value prefilled.
  validateSearch: (search: Record<string, unknown>): { snapshot?: string; volume?: string } => ({
    snapshot: typeof search.snapshot === 'string' ? search.snapshot : undefined,
    volume: typeof search.volume === 'string' ? search.volume : undefined,
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
  const volumes = useQuery({
    queryKey: ['volumes'],
    queryFn: () => listAll<Volume>('/volumes'),
  })

  const gpuModels = Array.from(
    new Set(
      (capacity.data?.items ?? []).flatMap((node) =>
        node.gpus.filter((gpu) => gpu.total > 0).map((gpu) => gpu.model),
      ),
    ),
  )
  const attachable = (volumes.data ?? []).filter((volume) => volume.state === 'ready')

  const form = useForm({
    defaultValues: {
      name: '',
      snapshot: prefill.snapshot ?? '',
      cpu: '1',
      memoryMiB: '512',
      gpuCount: '0',
      gpuModel: '',
      volumes: prefill.volume ? [{ volume: prefill.volume, path: '/workspace' }] : [],
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
            <CardTitle>{m.label_volumes()}</CardTitle>
            <CardDescription>{m.new_volumes_description()}</CardDescription>
          </CardHeader>
          <CardContent>
            <FieldGroup>
              <form.Field name="volumes" mode="array">
                {(mounts) => (
                  <>
                    {mounts.state.value.map((mount, i) => {
                      const earlier = mounts.state.value.slice(0, i)
                      // The same rules the server applies, shown as you type.
                      const problem =
                        mount.path.trim() &&
                        volumeMountProblem(
                          mount.path.trim(),
                          earlier.map((other) => other.path.trim()),
                        )
                      return (
                        <div key={i} className="grid grid-cols-[1fr_1fr_auto] items-start gap-2">
                          <form.Field name={`volumes[${i}].volume`}>
                            {(field) => (
                              <Field>
                                <FieldLabel htmlFor={`volume-${i}`} className="sr-only">
                                  {m.label_volume()}
                                </FieldLabel>
                                <NativeSelect
                                  id={`volume-${i}`}
                                  className="w-full"
                                  value={field.state.value}
                                  onChange={(e) => field.handleChange(e.target.value)}
                                >
                                  <NativeSelectOption value="">{m.new_volume_choose()}</NativeSelectOption>
                                  {attachable.map((volume) => (
                                    <NativeSelectOption
                                      key={volume.id}
                                      value={volume.id}
                                      disabled={volume.held_by !== null}
                                    >
                                      {volume.held_by
                                        ? m.new_volume_in_use({ name: volume.name })
                                        : volume.name}
                                    </NativeSelectOption>
                                  ))}
                                </NativeSelect>
                              </Field>
                            )}
                          </form.Field>
                          <form.Field name={`volumes[${i}].path`}>
                            {(field) => (
                              <Field data-invalid={problem ? true : undefined}>
                                <FieldLabel htmlFor={`volume-path-${i}`} className="sr-only">
                                  {m.new_volume_path()}
                                </FieldLabel>
                                <Input
                                  id={`volume-path-${i}`}
                                  placeholder="/data"
                                  aria-invalid={problem ? true : undefined}
                                  value={field.state.value}
                                  onChange={(e) => field.handleChange(e.target.value)}
                                />
                                {problem && <FieldError>{problem}</FieldError>}
                              </Field>
                            )}
                          </form.Field>
                          <Button
                            type="button"
                            variant="ghost"
                            size="icon"
                            aria-label={m.new_volume_remove()}
                            onClick={() => mounts.removeValue(i)}
                          >
                            <X />
                          </Button>
                        </div>
                      )
                    })}
                    <div>
                      <Button
                        type="button"
                        variant="outline"
                        size="sm"
                        disabled={mounts.state.value.length >= 16}
                        onClick={() =>
                          mounts.pushValue({
                            volume: '',
                            path: mounts.state.value.length === 0 ? '/workspace' : '',
                          })
                        }
                      >
                        <Plus data-icon="inline-start" />
                        {m.new_volume_add()}
                      </Button>
                    </div>
                  </>
                )}
              </form.Field>
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
