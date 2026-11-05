import { useQuery } from '@tanstack/react-query'
import { createFileRoute } from '@tanstack/react-router'
import { Filter } from 'lucide-react'
import { useState, type ChangeEvent } from 'react'

import { api, type AuditRecord, type ListResponse } from '@/api'
import { PageHeader } from '@/components/page-header'
import { Problem } from '@/components/problem'
import { Alert, AlertDescription } from '@/components/ui/alert'
import {
  Breadcrumb,
  BreadcrumbItem,
  BreadcrumbList,
  BreadcrumbPage,
  BreadcrumbSeparator,
} from '@/components/ui/breadcrumb'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Field, FieldGroup, FieldLabel } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { Item, ItemContent, ItemDescription, ItemGroup, ItemTitle } from '@/components/ui/item'
import { Spinner } from '@/components/ui/spinner'
import { auditPath, toRfc3339, type AuditFilters } from '@/lib/audit'
import { useLocale } from '@/lib/i18n'
import { formatLocalDateTime } from '@/lib/tokens'
import { m } from '@/paraglide/messages'

export const Route = createFileRoute('/_authed/admin/audit')({
  component: AuditPage,
})

const EMPTY: AuditFilters = { actor: '', action: '', target: '', from: '', to: '' }

function AuditPage() {
  useLocale() // re-render on a language switch (see lib/i18n.tsx)
  const [draft, setDraft] = useState<AuditFilters>(EMPTY)
  const [applied, setApplied] = useState<AuditFilters>(EMPTY)
  // The cursor is opaque and only ever comes from the server's next_cursor.
  // Applying a new filter starts again at the newest record.
  const [cursor, setCursor] = useState<string | null>(null)

  const records = useQuery({
    queryKey: ['audit', applied, cursor],
    queryFn: () => api<ListResponse<AuditRecord>>('GET', auditPath(applied, cursor)),
  })

  const field = (key: keyof AuditFilters) => ({
    value: draft[key],
    onChange: (e: ChangeEvent<HTMLInputElement>) =>
      setDraft((current) => ({ ...current, [key]: e.target.value })),
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
                <BreadcrumbPage>{m.audit_title()}</BreadcrumbPage>
              </BreadcrumbItem>
            </BreadcrumbList>
          </Breadcrumb>
        }
        title={m.audit_title()}
        description={m.audit_description()}
      />

      <Card>
        <CardHeader>
          <CardTitle>{m.audit_filters()}</CardTitle>
          <CardDescription>{m.audit_filters_description()}</CardDescription>
        </CardHeader>
        <CardContent>
          <form
            onSubmit={(e) => {
              e.preventDefault()
              setApplied({
                actor: draft.actor.trim(),
                action: draft.action.trim(),
                target: draft.target.trim(),
                from: toRfc3339(draft.from),
                to: toRfc3339(draft.to),
              })
              setCursor(null)
            }}
          >
            <FieldGroup className="grid gap-3 sm:grid-cols-2 lg:grid-cols-5">
              <Field>
                <FieldLabel htmlFor="audit-actor">{m.audit_actor()}</FieldLabel>
                <Input id="audit-actor" placeholder="admin" {...field('actor')} />
              </Field>
              <Field>
                <FieldLabel htmlFor="audit-action">{m.audit_action()}</FieldLabel>
                <Input id="audit-action" placeholder="user.create" {...field('action')} />
              </Field>
              <Field>
                <FieldLabel htmlFor="audit-target">{m.audit_target()}</FieldLabel>
                <Input
                  id="audit-target"
                  placeholder={m.audit_target_placeholder()}
                  {...field('target')}
                />
              </Field>
              <Field>
                <FieldLabel htmlFor="audit-from">{m.audit_from()}</FieldLabel>
                <Input id="audit-from" type="datetime-local" {...field('from')} />
              </Field>
              <Field>
                <FieldLabel htmlFor="audit-to">{m.audit_to()}</FieldLabel>
                <Input id="audit-to" type="datetime-local" {...field('to')} />
              </Field>
              <Field orientation="horizontal" className="sm:col-span-2 lg:col-span-5">
                <Button type="submit" variant="outline">
                  <Filter data-icon="inline-start" />
                  {m.audit_apply()}
                </Button>
              </Field>
            </FieldGroup>
          </form>
        </CardContent>
      </Card>

      {records.isPending && (
        <div className="flex items-center gap-2 py-8 text-sm text-muted-foreground">
          <Spinner />
          {m.audit_loading()}
        </div>
      )}
      {records.error && (
        <Alert variant="destructive">
          <AlertDescription>
            {m.audit_load_error()}: <Problem error={records.error} />
          </AlertDescription>
        </Alert>
      )}
      {records.data?.items.length === 0 && (
        <p className="text-sm text-muted-foreground">{m.audit_empty()}</p>
      )}

      {records.data && records.data.items.length > 0 && (
        <ItemGroup className="gap-2">
          {records.data.items.map((record) => (
            <Item key={record.id} variant="outline" role="listitem" data-testid="audit-row">
              <ItemContent className="min-w-0">
                <ItemTitle>
                  <span className="font-mono text-xs">{record.action}</span>
                  <span className="text-xs font-normal text-muted-foreground">
                    {formatLocalDateTime(record.at)}
                  </span>
                </ItemTitle>
                <ItemDescription>
                  {record.actor_name ?? '—'} · {record.target_type}{' '}
                  <span className="font-mono">{record.target_id}</span>
                </ItemDescription>
                {![undefined, null, '{}'].includes(JSON.stringify(record.detail)) && (
                  <pre className="max-w-full overflow-x-auto font-mono text-xs text-muted-foreground">
                    {JSON.stringify(record.detail)}
                  </pre>
                )}
              </ItemContent>
            </Item>
          ))}
        </ItemGroup>
      )}

      <div className="flex gap-2">
        {cursor && (
          <Button variant="outline" onClick={() => setCursor(null)}>
            {m.audit_newest()}
          </Button>
        )}
        {records.data?.next_cursor && (
          <Button variant="outline" onClick={() => setCursor(records.data?.next_cursor ?? null)}>
            {m.audit_older()}
          </Button>
        )}
      </div>
    </div>
  )
}
