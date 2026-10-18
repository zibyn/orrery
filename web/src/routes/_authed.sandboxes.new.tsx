import { useForm } from '@tanstack/react-form'
import { useQuery } from '@tanstack/react-query'
import { createFileRoute, useNavigate } from '@tanstack/react-router'
import { useState } from 'react'

import {
  api,
  type CapacityNode,
  type CreateSandboxRequest,
  type ListResponse,
  type Sandbox,
  type Snapshot,
  type Workspace,
} from '@/api'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { buildCreateSandboxBody, createSandboxSchema } from '@/lib/sandbox-form'

export const Route = createFileRoute('/_authed/sandboxes/new')({
  component: NewSandboxPage,
})

const selectClass =
  'flex h-9 w-full rounded-md border border-input bg-transparent px-3 py-1 text-sm shadow-xs focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring disabled:opacity-50'

function NewSandboxPage() {
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
        setError(parsed.error.issues[0]?.message ?? 'Invalid input')
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
        setError(err instanceof Error ? err.message : 'Could not create the Sandbox')
      }
    },
  })

  return (
    <div className="flex max-w-2xl flex-col gap-6">
      <div>
        <h1 className="text-2xl font-semibold">New Sandbox</h1>
        <p className="text-sm text-muted-foreground">
          Resources, Secrets and labels stay CLI-only (api.md §8).
        </p>
      </div>

      <form
        className="flex flex-col gap-4"
        onSubmit={(e) => {
          e.preventDefault()
          form.handleSubmit()
        }}
      >
        <form.Field name="name">
          {(field) => (
            <div className="flex flex-col gap-2">
              <Label htmlFor={field.name}>Name</Label>
              <Input
                id={field.name}
                value={field.state.value}
                onChange={(e) => field.handleChange(e.target.value)}
                autoFocus
              />
            </div>
          )}
        </form.Field>

        <form.Field name="snapshot">
          {(field) => (
            <div className="flex flex-col gap-2">
              <Label htmlFor={field.name}>Snapshot</Label>
              <select
                id={field.name}
                className={selectClass}
                value={field.state.value}
                onChange={(e) => field.handleChange(e.target.value)}
              >
                <option value="">Deployment default</option>
                {snapshots.data?.items.map((snapshot) => (
                  <option key={snapshot.id} value={snapshot.name}>
                    {snapshot.name}
                    {snapshot.owner_id === null ? ' (platform)' : ''}
                  </option>
                ))}
              </select>
            </div>
          )}
        </form.Field>

        <div className="grid grid-cols-2 gap-4">
          <form.Field name="cpu">
            {(field) => (
              <div className="flex flex-col gap-2">
                <Label htmlFor={field.name}>CPU</Label>
                <Input
                  id={field.name}
                  type="number"
                  min="0.1"
                  step="0.1"
                  value={field.state.value}
                  onChange={(e) => field.handleChange(e.target.value)}
                />
              </div>
            )}
          </form.Field>
          <form.Field name="memoryMiB">
            {(field) => (
              <div className="flex flex-col gap-2">
                <Label htmlFor={field.name}>Memory (MiB)</Label>
                <Input
                  id={field.name}
                  type="number"
                  min="1"
                  step="1"
                  value={field.state.value}
                  onChange={(e) => field.handleChange(e.target.value)}
                />
              </div>
            )}
          </form.Field>
        </div>

        <div className="grid grid-cols-2 gap-4">
          <form.Field name="gpuCount">
            {(field) => (
              <div className="flex flex-col gap-2">
                <Label htmlFor={field.name}>GPUs</Label>
                <Input
                  id={field.name}
                  type="number"
                  min="0"
                  step="1"
                  value={field.state.value}
                  onChange={(e) => field.handleChange(e.target.value)}
                />
              </div>
            )}
          </form.Field>
          <form.Field name="gpuModel">
            {(field) => (
              <div className="flex flex-col gap-2">
                <Label htmlFor={field.name}>GPU model</Label>
                <select
                  id={field.name}
                  className={selectClass}
                  value={field.state.value}
                  onChange={(e) => field.handleChange(e.target.value)}
                >
                  <option value="">Any model</option>
                  {gpuModels.map((model) => (
                    <option key={model} value={model}>
                      {model}
                    </option>
                  ))}
                </select>
              </div>
            )}
          </form.Field>
        </div>

        <form.Field name="workspaceId">
          {(field) => (
            <div className="flex flex-col gap-2">
              <Label htmlFor={field.name}>Workspace</Label>
              <select
                id={field.name}
                className={selectClass}
                value={field.state.value}
                onChange={(e) => field.handleChange(e.target.value)}
              >
                <option value="">Ephemeral (created with the Sandbox)</option>
                {attachable.map((workspace) => (
                  <option key={workspace.id} value={workspace.id}>
                    {workspace.id}
                  </option>
                ))}
              </select>
            </div>
          )}
        </form.Field>

        <form.Subscribe selector={(state) => state.values.workspaceId}>
          {(workspaceId) =>
            workspaceId ? (
              <form.Field name="workspaceMode">
                {(field) => (
                  <div className="flex flex-col gap-2">
                    <Label htmlFor={field.name}>Workspace mode</Label>
                    <select
                      id={field.name}
                      className={selectClass}
                      value={field.state.value}
                      onChange={(e) => field.handleChange(e.target.value as 'rw' | 'ro')}
                    >
                      <option value="rw">Read-write</option>
                      <option value="ro">Read-only</option>
                    </select>
                  </div>
                )}
              </form.Field>
            ) : null
          }
        </form.Subscribe>

        <form.Field name="idleStop">
          {(field) => (
            <div className="flex flex-col gap-2">
              <Label htmlFor={field.name}>Idle stop (seconds, 0 = off)</Label>
              <Input
                id={field.name}
                type="number"
                min="0"
                step="1"
                value={field.state.value}
                onChange={(e) => field.handleChange(e.target.value)}
              />
            </div>
          )}
        </form.Field>

        {(error || snapshots.error || capacity.error || workspaces.error) && (
          <p className="text-sm text-destructive">
            {error ||
              snapshots.error?.message ||
              capacity.error?.message ||
              workspaces.error?.message}
          </p>
        )}
        <div className="flex gap-2">
          <Button type="submit" disabled={form.state.isSubmitting}>
            Create Sandbox
          </Button>
          <Button type="button" variant="outline" onClick={() => navigate({ to: '/sandboxes' })}>
            Cancel
          </Button>
        </div>
      </form>
    </div>
  )
}
