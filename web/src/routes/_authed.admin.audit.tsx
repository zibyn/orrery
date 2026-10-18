import { useQuery } from '@tanstack/react-query'
import { createFileRoute } from '@tanstack/react-router'
import { useState, type ChangeEvent } from 'react'

import { api, type AuditRecord, type ListResponse } from '@/api'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { auditPath, toRfc3339, type AuditFilters } from '@/lib/audit'

export const Route = createFileRoute('/_authed/admin/audit')({
  component: AuditPage,
})

const EMPTY: AuditFilters = { actor: '', action: '', target: '', from: '', to: '' }

function AuditPage() {
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
      <div>
        <h1 className="text-2xl font-semibold">Audit</h1>
        <p className="text-sm text-muted-foreground">
          Newest first. Filter by actor (User id or name), action, target and time range.
        </p>
      </div>

      <form
        className="grid grid-cols-2 gap-4 rounded-md border border-border p-4 md:grid-cols-5"
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
        <div className="flex flex-col gap-2">
          <Label htmlFor="audit-actor">Actor</Label>
          <Input id="audit-actor" placeholder="admin" {...field('actor')} />
        </div>
        <div className="flex flex-col gap-2">
          <Label htmlFor="audit-action">Action</Label>
          <Input id="audit-action" placeholder="user.create" {...field('action')} />
        </div>
        <div className="flex flex-col gap-2">
          <Label htmlFor="audit-target">Target</Label>
          <Input id="audit-target" placeholder="target id" {...field('target')} />
        </div>
        <div className="flex flex-col gap-2">
          <Label htmlFor="audit-from">From</Label>
          <Input id="audit-from" type="datetime-local" {...field('from')} />
        </div>
        <div className="flex flex-col gap-2">
          <Label htmlFor="audit-to">To</Label>
          <Input id="audit-to" type="datetime-local" {...field('to')} />
        </div>
        <div className="col-span-full">
          <Button type="submit">Apply filters</Button>
        </div>
      </form>

      {records.isPending && <p className="text-sm text-muted-foreground">Loading…</p>}
      {records.error && (
        <p className="text-sm text-destructive">Could not load the audit log: {records.error.message}</p>
      )}
      {records.data?.items.length === 0 && (
        <p className="text-sm text-muted-foreground">No audit records match.</p>
      )}

      {records.data && records.data.items.length > 0 && (
        <div className="overflow-x-auto rounded-md border border-border">
          <table className="w-full border-collapse text-sm">
            <thead>
              <tr className="border-b border-border text-left text-muted-foreground">
                <th className="p-2 font-medium">At</th>
                <th className="p-2 font-medium">Actor</th>
                <th className="p-2 font-medium">Action</th>
                <th className="p-2 font-medium">Target</th>
                <th className="p-2 font-medium">Detail</th>
              </tr>
            </thead>
            <tbody>
              {records.data.items.map((record) => (
                <tr key={record.id} className="border-b border-border align-top" data-testid="audit-row">
                  <td className="whitespace-nowrap p-2">{new Date(record.at).toLocaleString()}</td>
                  <td className="p-2">{record.actor_name ?? '—'}</td>
                  <td className="whitespace-nowrap p-2 font-mono text-xs">{record.action}</td>
                  <td className="p-2">
                    <span className="block">{record.target_type}</span>
                    <span className="block font-mono text-xs text-muted-foreground">
                      {record.target_id}
                    </span>
                  </td>
                  <td className="p-2">
                    <pre className="max-w-md overflow-x-auto font-mono text-xs">
                      {JSON.stringify(record.detail)}
                    </pre>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      <div className="flex gap-2">
        {cursor && (
          <Button variant="outline" onClick={() => setCursor(null)}>
            Newest
          </Button>
        )}
        {records.data?.next_cursor && (
          <Button variant="outline" onClick={() => setCursor(records.data?.next_cursor ?? null)}>
            Older records
          </Button>
        )}
      </div>
    </div>
  )
}
