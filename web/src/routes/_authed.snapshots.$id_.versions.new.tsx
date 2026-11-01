import { useForm } from '@tanstack/react-form'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { createFileRoute, useNavigate } from '@tanstack/react-router'
import { Plus, Upload, X } from 'lucide-react'
import { useState, type ReactNode } from 'react'

import { api, type ListResponse, type Secret, type Snapshot, type SnapshotVersion } from '@/api'
import { Breadcrumbs } from '@/components/breadcrumbs'
import { PageHeader } from '@/components/page-header'
import { Problem } from '@/components/problem'
import { Button, buttonVariants } from '@/components/ui/button'
import { Card, CardContent, CardFooter } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { useLocale } from '@/lib/i18n'
import { buildSnapshotVersionBody, snapshotVersionSchema } from '@/lib/snapshot-form'
import { cn, selectClass } from '@/lib/utils'
import { m } from '@/paraglide/messages'

export const Route = createFileRoute('/_authed/snapshots/$id_/versions/new')({
  component: NewSnapshotVersionPage,
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

function NewSnapshotVersionPage() {
  useLocale() // re-render on a language switch (see lib/i18n.tsx)
  const { id } = Route.useParams()
  const navigate = useNavigate()
  const queryClient = useQueryClient()
  const [error, setError] = useState<unknown>()
  // One key per form mount: repeated submits cannot create a second version (api.md §2).
  const [idempotencyKey] = useState(() => crypto.randomUUID())

  const snapshot = useQuery({
    queryKey: ['snapshot', id],
    queryFn: () => api<Snapshot>('GET', `/snapshots/${id}`),
  })
  const snapshots = useQuery({
    queryKey: ['snapshots'],
    queryFn: () => api<ListResponse<Snapshot>>('GET', '/snapshots?limit=200'),
  })
  const secrets = useQuery({
    queryKey: ['secrets'],
    queryFn: () => api<ListResponse<Secret>>('GET', '/secrets'),
  })

  const form = useForm({
    defaultValues: {
      source: 'build' as 'build' | 'import',
      dockerfile: '',
      base: '',
      secrets: [] as { id: string; name: string }[],
      ref: '',
      authSecret: '',
    },
    onSubmit: async ({ value }) => {
      setError(undefined)
      const parsed = snapshotVersionSchema.safeParse(value)
      if (!parsed.success) {
        setError(parsed.error.issues[0]?.message ?? m.common_invalid_input())
        return
      }
      try {
        const created = await api<SnapshotVersion>(
          'POST',
          `/snapshots/${id}/versions`,
          buildSnapshotVersionBody(parsed.data),
          { 'Idempotency-Key': idempotencyKey },
        )
        await queryClient.invalidateQueries({ queryKey: ['snapshot-versions', id] })
        await queryClient.invalidateQueries({ queryKey: ['snapshot', id] })
        void navigate({ to: '/snapshots/$id', params: { id }, search: { version: created.id } })
      } catch (err) {
        setError(err)
      }
    },
  })

  const secretOptions = secrets.data?.items.map((secret) => (
    <option key={secret.name} value={secret.name}>
      {secret.name}
    </option>
  ))
  const loadError = snapshot.error ?? snapshots.error ?? secrets.error
  const name = snapshot.data?.name ?? id

  return (
    <div className="flex max-w-3xl flex-col gap-6">
      <PageHeader
        above={
          <Breadcrumbs
            items={[
              { label: m.nav_snapshots(), to: '/snapshots' },
              { label: name, to: '/snapshots/$id', params: { id } },
              { label: m.snapshot_new_version() },
            ]}
          />
        }
        title={m.snapshot_new_version()}
        description={m.version_new_description()}
      />

      <form
        className="flex flex-col gap-5"
        onSubmit={(e) => {
          e.preventDefault()
          void form.handleSubmit()
        }}
      >
        <Card>
          <CardContent className="flex flex-col gap-4">
            <form.Field name="source">
              {(field) => (
                <Field label={m.version_new_source()} htmlFor={field.name}>
                  <select
                    id={field.name}
                    className={selectClass}
                    value={field.state.value}
                    onChange={(e) => field.handleChange(e.target.value as 'build' | 'import')}
                  >
                    <option value="build">{m.version_new_build()}</option>
                    <option value="import">{m.version_new_import()}</option>
                  </select>
                </Field>
              )}
            </form.Field>

            <form.Subscribe selector={(state) => state.values.source}>
              {(source) =>
                source === 'build' ? (
                  <>
                    <form.Field name="dockerfile">
                      {(field) => (
                        <div className="flex flex-col gap-1.5">
                          <div className="flex items-center justify-between gap-2">
                            <Label htmlFor={field.name}>{m.version_new_dockerfile()}</Label>
                            <label
                              className={cn(
                                buttonVariants({ variant: 'outline', size: 'sm' }),
                                'cursor-pointer',
                              )}
                            >
                              <Upload />
                              {m.version_new_load_file()}
                              <input
                                type="file"
                                className="sr-only"
                                onChange={async (e) => {
                                  const file = e.target.files?.[0]
                                  if (file) field.handleChange(await file.text())
                                  e.target.value = ''
                                }}
                              />
                            </label>
                          </div>
                          <textarea
                            id={field.name}
                            rows={14}
                            spellCheck={false}
                            className="min-h-40 w-full rounded-lg border border-input bg-transparent px-2.5 py-2 font-mono text-xs outline-none focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/50 dark:bg-input/30"
                            value={field.state.value}
                            onChange={(e) => field.handleChange(e.target.value)}
                            placeholder={'FROM base\nRUN …'}
                          />
                          <p className="text-xs text-muted-foreground">
                            {m.version_new_contexts_hint()}
                          </p>
                        </div>
                      )}
                    </form.Field>

                    <form.Field name="base">
                      {(field) => (
                        <Field
                          label={m.version_new_base()}
                          htmlFor={field.name}
                          hint={m.version_new_base_hint()}
                        >
                          <select
                            id={field.name}
                            className={selectClass}
                            value={field.state.value}
                            onChange={(e) => field.handleChange(e.target.value)}
                          >
                            <option value="">{m.version_new_base_none()}</option>
                            {snapshots.data?.items.map((item) => (
                              <option key={item.id} value={item.name}>
                                {item.name}
                                {item.owner_id === null ? m.new_platform_suffix() : ''}
                              </option>
                            ))}
                          </select>
                        </Field>
                      )}
                    </form.Field>

                    <form.Field name="secrets" mode="array">
                      {(rows) => (
                        <div className="flex flex-col gap-2">
                          <Label>{m.version_new_secrets()}</Label>
                          {rows.state.value.map((_, index) => (
                            <div key={index} className="flex flex-wrap gap-2 sm:flex-nowrap">
                              <form.Field name={`secrets[${index}].id`}>
                                {(field) => (
                                  <Input
                                    aria-label={m.version_new_secret_id()}
                                    placeholder={m.version_new_secret_id()}
                                    className="font-mono"
                                    value={field.state.value}
                                    onChange={(e) => field.handleChange(e.target.value)}
                                  />
                                )}
                              </form.Field>
                              <form.Field name={`secrets[${index}].name`}>
                                {(field) => (
                                  <select
                                    aria-label={m.version_new_secret()}
                                    className={selectClass}
                                    value={field.state.value}
                                    onChange={(e) => field.handleChange(e.target.value)}
                                  >
                                    <option value="">{m.version_new_secret_choose()}</option>
                                    {secretOptions}
                                  </select>
                                )}
                              </form.Field>
                              <Button
                                type="button"
                                variant="ghost"
                                size="icon"
                                aria-label={m.version_new_remove_secret()}
                                onClick={() => rows.removeValue(index)}
                              >
                                <X />
                              </Button>
                            </div>
                          ))}
                          <div>
                            <Button
                              type="button"
                              variant="outline"
                              size="sm"
                              onClick={() => rows.pushValue({ id: '', name: '' })}
                            >
                              <Plus />
                              {m.version_new_add_secret()}
                            </Button>
                          </div>
                          <p className="text-xs text-muted-foreground">
                            {m.version_new_secrets_hint()}
                          </p>
                        </div>
                      )}
                    </form.Field>
                  </>
                ) : (
                  <>
                    <form.Field name="ref">
                      {(field) => (
                        <Field
                          label={m.version_new_ref()}
                          htmlFor={field.name}
                          hint={m.version_new_ref_hint()}
                        >
                          <Input
                            id={field.name}
                            className="font-mono"
                            value={field.state.value}
                            onChange={(e) => field.handleChange(e.target.value)}
                            placeholder={m.version_new_ref_placeholder()}
                          />
                        </Field>
                      )}
                    </form.Field>
                    <form.Field name="authSecret">
                      {(field) => (
                        <Field label={m.version_new_auth_secret()} htmlFor={field.name}>
                          <select
                            id={field.name}
                            className={selectClass}
                            value={field.state.value}
                            onChange={(e) => field.handleChange(e.target.value)}
                          >
                            <option value="">{m.version_new_auth_none()}</option>
                            {secretOptions}
                          </select>
                        </Field>
                      )}
                    </form.Field>
                  </>
                )
              }
            </form.Subscribe>
          </CardContent>
          <CardFooter className="justify-end">
            <Button
              type="button"
              variant="ghost"
              onClick={() => navigate({ to: '/snapshots/$id', params: { id } })}
            >
              {m.common_cancel()}
            </Button>
            <form.Subscribe selector={(state) => state.isSubmitting}>
              {(submitting) => (
                <Button type="submit" disabled={submitting}>
                  {submitting ? m.version_new_submitting() : m.version_new_submit()}
                </Button>
              )}
            </form.Subscribe>
          </CardFooter>
        </Card>

        {(error !== undefined || loadError) && (
          <p className="text-sm text-destructive" role="alert">
            <Problem error={error ?? loadError} />
          </p>
        )}
      </form>
    </div>
  )
}
