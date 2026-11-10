import { useForm } from '@tanstack/react-form'
import { useQuery } from '@tanstack/react-query'
import { Plus, Upload, X } from 'lucide-react'
import { useState, type ReactNode } from 'react'

import { api, type ListResponse, type Secret, type Snapshot } from '@/api'
import { notifyError } from '@/lib/notify'
import { Button, buttonVariants } from '@/components/ui/button'
import { Card, CardContent, CardFooter } from '@/components/ui/card'
import {
  Field,
  FieldDescription,
  FieldError,
  FieldGroup,
  FieldLabel,
  FieldLegend,
  FieldSet,
} from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { NativeSelect, NativeSelectOption } from '@/components/ui/native-select'
import { Textarea } from '@/components/ui/textarea'
import {
  snapshotVersionSchema,
  type SnapshotVersionFormFields,
  type SnapshotVersionFormValues,
} from '@/lib/snapshot-form'
import { baseSnapshotOptions } from '@/lib/snapshots'
import { cn } from '@/lib/utils'
import { m } from '@/paraglide/messages'

// The build-or-import form shared by a new version and a new Snapshot. A
// rejected onSubmit shows its error under the form.
export function SnapshotVersionForm({
  initial,
  snapshotId,
  above,
  onSubmit,
  onCancel,
}: {
  initial: SnapshotVersionFormFields
  // The Snapshot itself is not offered as its own base.
  snapshotId?: string
  above?: ReactNode
  onSubmit: (values: SnapshotVersionFormValues) => Promise<void>
  onCancel: () => void
}) {
  const [error, setError] = useState<string>()
  const snapshots = useQuery({
    queryKey: ['snapshots'],
    queryFn: () => api<ListResponse<Snapshot>>('GET', '/snapshots?limit=200'),
  })
  const secrets = useQuery({
    queryKey: ['secrets'],
    queryFn: () => api<ListResponse<Secret>>('GET', '/secrets'),
  })

  const form = useForm({
    defaultValues: initial,
    onSubmit: async ({ value }) => {
      setError(undefined)
      const parsed = snapshotVersionSchema.safeParse(value)
      if (!parsed.success) {
        setError(parsed.error.issues[0]?.message ?? m.common_invalid_input())
        return
      }
      try {
        await onSubmit(parsed.data)
      } catch (err) {
        notifyError(err)
      }
    },
  })

  const secretOptions = secrets.data?.items.map((secret) => (
    <NativeSelectOption key={secret.name} value={secret.name}>
      {secret.name}
    </NativeSelectOption>
  ))

  return (
    <form
      className="flex flex-col gap-5"
      onSubmit={(e) => {
        e.preventDefault()
        void form.handleSubmit()
      }}
    >
      <Card>
        <CardContent>
          <FieldGroup>
            {above}
            <form.Field name="source">
              {(field) => (
                <Field>
                  <FieldLabel htmlFor={field.name}>{m.version_new_source()}</FieldLabel>
                  <NativeSelect
                    id={field.name}
                    value={field.state.value}
                    onChange={(e) => field.handleChange(e.target.value as 'build' | 'import')}
                  >
                    <NativeSelectOption value="build">{m.version_new_build()}</NativeSelectOption>
                    <NativeSelectOption value="import">{m.version_new_import()}</NativeSelectOption>
                  </NativeSelect>
                </Field>
              )}
            </form.Field>

            <form.Subscribe selector={(state) => state.values.source}>
              {(source) =>
                source === 'build' ? (
                  <>
                    <form.Field name="dockerfile">
                      {(field) => (
                        <Field>
                          <div className="flex items-center justify-between gap-2">
                            <FieldLabel htmlFor={field.name}>{m.version_new_dockerfile()}</FieldLabel>
                            <label
                              className={cn(
                                buttonVariants({ variant: 'outline', size: 'sm' }),
                                'cursor-pointer',
                              )}
                            >
                              <Upload data-icon="inline-start" />
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
                          <Textarea
                            id={field.name}
                            rows={14}
                            spellCheck={false}
                            className="min-h-40 font-mono"
                            value={field.state.value}
                            onChange={(e) => field.handleChange(e.target.value)}
                            placeholder={'FROM base\nRUN …'}
                          />
                          <FieldDescription>{m.version_new_contexts_hint()}</FieldDescription>
                          <form.Subscribe selector={(state) => state.values.contexts}>
                            {(contexts) =>
                              Object.keys(contexts).length > 0 && (
                                <FieldDescription>
                                  {m.version_new_contexts_kept({
                                    names: Object.keys(contexts).join(', '),
                                  })}
                                </FieldDescription>
                              )
                            }
                          </form.Subscribe>
                        </Field>
                      )}
                    </form.Field>

                    <form.Field name="base">
                      {(field) => (
                        <Field>
                          <FieldLabel htmlFor={field.name}>{m.version_new_base()}</FieldLabel>
                          <NativeSelect
                            id={field.name}
                            value={field.state.value}
                            onChange={(e) => field.handleChange(e.target.value)}
                          >
                            <NativeSelectOption value="">{m.version_new_base_none()}</NativeSelectOption>
                            {baseSnapshotOptions(snapshots.data?.items ?? [], snapshotId).map((item) => (
                              <NativeSelectOption key={item.id} value={item.name}>
                                {item.name}
                                {item.owner_id === null ? m.new_platform_suffix() : ''}
                              </NativeSelectOption>
                            ))}
                          </NativeSelect>
                          <FieldDescription>{m.version_new_base_hint()}</FieldDescription>
                        </Field>
                      )}
                    </form.Field>

                    <form.Field name="secrets" mode="array">
                      {(rows) => (
                        <FieldSet>
                          <FieldLegend variant="label">{m.version_new_secrets()}</FieldLegend>
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
                                  <NativeSelect
                                    aria-label={m.version_new_secret()}
                                    className="w-full"
                                    value={field.state.value}
                                    onChange={(e) => field.handleChange(e.target.value)}
                                  >
                                    <NativeSelectOption value="">
                                      {m.version_new_secret_choose()}
                                    </NativeSelectOption>
                                    {secretOptions}
                                  </NativeSelect>
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
                              <Plus data-icon="inline-start" />
                              {m.version_new_add_secret()}
                            </Button>
                          </div>
                          <FieldDescription>{m.version_new_secrets_hint()}</FieldDescription>
                        </FieldSet>
                      )}
                    </form.Field>
                  </>
                ) : (
                  <>
                    <form.Field name="ref">
                      {(field) => (
                        <Field>
                          <FieldLabel htmlFor={field.name}>{m.version_new_ref()}</FieldLabel>
                          <Input
                            id={field.name}
                            className="font-mono"
                            value={field.state.value}
                            onChange={(e) => field.handleChange(e.target.value)}
                            placeholder={m.version_new_ref_placeholder()}
                          />
                          <FieldDescription>{m.version_new_ref_hint()}</FieldDescription>
                        </Field>
                      )}
                    </form.Field>
                    <form.Field name="authSecret">
                      {(field) => (
                        <Field>
                          <FieldLabel htmlFor={field.name}>{m.version_new_auth_secret()}</FieldLabel>
                          <NativeSelect
                            id={field.name}
                            value={field.state.value}
                            onChange={(e) => field.handleChange(e.target.value)}
                          >
                            <NativeSelectOption value="">{m.version_new_auth_none()}</NativeSelectOption>
                            {secretOptions}
                          </NativeSelect>
                        </Field>
                      )}
                    </form.Field>
                  </>
                )
              }
            </form.Subscribe>
          </FieldGroup>
        </CardContent>
        <CardFooter className="justify-end">
          <Button
            type="button"
            variant="ghost"
            onClick={onCancel}
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

      {error && (
        <FieldError>
          {error}
        </FieldError>
      )}
    </form>
  )
}
