import { useQuery } from '@tanstack/react-query'
import { createFileRoute } from '@tanstack/react-router'
import { Filter, ScrollText } from 'lucide-react'
import { useState, type ChangeEvent } from 'react'

import { api, type AuditRecord, type ListResponse } from '@/api'
import { Breadcrumbs } from '@/components/breadcrumbs'
import { ListCard, ListRow } from '@/components/list'
import { PageHeader } from '@/components/page-header'
import { Problem } from '@/components/problem'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
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
          <Breadcrumbs items={[{ label: m.nav_group_admin() }, { label: m.audit_title() }]} />
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
            className="grid gap-3 sm:grid-cols-2 lg:grid-cols-5"
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
            <div className="flex flex-col gap-1.5">
              <Label htmlFor="audit-actor">{m.audit_actor()}</Label>
              <Input id="audit-actor" placeholder="admin" {...field('actor')} />
            </div>
            <div className="flex flex-col gap-1.5">
              <Label htmlFor="audit-action">{m.audit_action()}</Label>
              <Input id="audit-action" placeholder="user.create" {...field('action')} />
            </div>
            <div className="flex flex-col gap-1.5">
              <Label htmlFor="audit-target">{m.audit_target()}</Label>
              <Input id="audit-target" placeholder="target id" {...field('target')} />
            </div>
            <div className="flex flex-col gap-1.5">
              <Label htmlFor="audit-from">{m.audit_from()}</Label>
              <Input id="audit-from" type="datetime-local" {...field('from')} />
            </div>
            <div className="flex flex-col gap-1.5">
              <Label htmlFor="audit-to">{m.audit_to()}</Label>
              <Input id="audit-to" type="datetime-local" {...field('to')} />
            </div>
            <div className="sm:col-span-2 lg:col-span-5">
              <Button type="submit" variant="outline">
                <Filter />
                {m.audit_apply()}
              </Button>
            </div>
          </form>
        </CardContent>
      </Card>

      {records.isPending && (
        <p className="py-8 text-sm text-muted-foreground">{m.audit_loading()}</p>
      )}
      {records.error && (
        <p className="text-sm text-destructive" role="alert">
          {m.audit_load_error()}: <Problem error={records.error} />
        </p>
      )}
      {records.data?.items.length === 0 && (
        <p className="text-sm text-muted-foreground">{m.audit_empty()}</p>
      )}

      {records.data && records.data.items.length > 0 && (
        <ListCard>
          {records.data.items.map((record) => (
            <ListRow
              key={record.id}
              data-testid="audit-row"
              icon={<ScrollText />}
              title={
                <>
                  <span className="font-mono text-xs">{record.action}</span>
                  <span className="text-xs font-normal text-muted-foreground">
                    {formatLocalDateTime(record.at)}
                  </span>
                </>
              }
              description={
                <>
                  <span>
                    {record.actor_name ?? '—'} · {record.target_type}{' '}
                    <span className="font-mono">{record.target_id}</span>
                  </span>
                  <pre className="mt-1 max-w-full overflow-x-auto font-mono">
                    {JSON.stringify(record.detail)}
                  </pre>
                </>
              }
            />
          ))}
        </ListCard>
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
