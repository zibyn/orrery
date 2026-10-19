import { useQuery } from '@tanstack/react-query'
import { createFileRoute } from '@tanstack/react-router'
import { Filter } from 'lucide-react'
import { useState, type ChangeEvent } from 'react'

import { api, type AuditRecord, type ListResponse } from '@/api'
import { PageHeader } from '@/components/page-header'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'
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
      <PageHeader
        title="Audit"
        description="Newest first. Filter by actor (User id or name), action, target and time range."
      />

      <Card>
        <CardHeader>
          <CardTitle>Filters</CardTitle>
          <CardDescription>Filters apply to the newest records first.</CardDescription>
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
              <Label htmlFor="audit-actor">Actor</Label>
              <Input id="audit-actor" placeholder="admin" {...field('actor')} />
            </div>
            <div className="flex flex-col gap-1.5">
              <Label htmlFor="audit-action">Action</Label>
              <Input id="audit-action" placeholder="user.create" {...field('action')} />
            </div>
            <div className="flex flex-col gap-1.5">
              <Label htmlFor="audit-target">Target</Label>
              <Input id="audit-target" placeholder="target id" {...field('target')} />
            </div>
            <div className="flex flex-col gap-1.5">
              <Label htmlFor="audit-from">From</Label>
              <Input id="audit-from" type="datetime-local" {...field('from')} />
            </div>
            <div className="flex flex-col gap-1.5">
              <Label htmlFor="audit-to">To</Label>
              <Input id="audit-to" type="datetime-local" {...field('to')} />
            </div>
            <div className="sm:col-span-2 lg:col-span-5">
              <Button type="submit" variant="outline">
                <Filter />
                Apply filters
              </Button>
            </div>
          </form>
        </CardContent>
      </Card>

      {records.isPending && <p className="py-8 text-sm text-muted-foreground">Loading audit log…</p>}
      {records.error && (
        <p className="text-sm text-destructive" role="alert">
          Could not load the audit log: {records.error.message}
        </p>
      )}
      {records.data?.items.length === 0 && (
        <p className="text-sm text-muted-foreground">No audit records match.</p>
      )}

      {records.data && records.data.items.length > 0 && (
        <Card className="overflow-hidden">
          <CardContent className="px-0 py-0">
            <Table>
              <TableHeader>
                <TableRow className="hover:bg-transparent">
                  <TableHead className="pl-5">At</TableHead>
                  <TableHead>Actor</TableHead>
                  <TableHead>Action</TableHead>
                  <TableHead>Target</TableHead>
                  <TableHead className="pr-5">Detail</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {records.data.items.map((record) => (
                  <TableRow key={record.id} className="align-top" data-testid="audit-row">
                    <TableCell className="pl-5 text-xs whitespace-nowrap">
                      {new Date(record.at).toLocaleString()}
                    </TableCell>
                    <TableCell className="text-sm">{record.actor_name ?? '—'}</TableCell>
                    <TableCell className="font-mono text-xs whitespace-nowrap">
                      {record.action}
                    </TableCell>
                    <TableCell>
                      <span className="block text-xs">{record.target_type}</span>
                      <span className="block font-mono text-xs text-muted-foreground">
                        {record.target_id}
                      </span>
                    </TableCell>
                    <TableCell className="pr-5">
                      <pre className="max-w-md overflow-x-auto font-mono text-xs text-muted-foreground">
                        {JSON.stringify(record.detail)}
                      </pre>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </CardContent>
        </Card>
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
