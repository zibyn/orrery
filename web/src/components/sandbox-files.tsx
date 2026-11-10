import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { ArrowUp, Download, File, Folder, PackagePlus, Upload } from 'lucide-react'
import { useRef, useState } from 'react'

import { api, apiRaw, type FileEntry, type ResourceVersion } from '@/api'
import { StatusBadge } from '@/components/status-badge'
import { Button, buttonVariants } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Empty, EmptyDescription, EmptyHeader } from '@/components/ui/empty'
import { Field, FieldLabel } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import {
  Item,
  ItemActions,
  ItemContent,
  ItemDescription,
  ItemGroup,
  ItemMedia,
  ItemTitle,
} from '@/components/ui/item'
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetFooter,
  SheetHeader,
  SheetTitle,
} from '@/components/ui/sheet'
import { Spinner } from '@/components/ui/spinner'
import {
  downloadHref,
  fileContentPath,
  filesPath,
  formatBytes,
  joinPath,
  parentPath,
} from '@/lib/files'
import { causeLabel } from '@/lib/labels'
import { resourceVersionPollInterval } from '@/lib/resources'
import { formatLocalDateTime } from '@/lib/tokens'
import { cn } from '@/lib/utils'
import { m } from '@/paraglide/messages'

export function SandboxFiles({
  sandboxId,
  workspaceId,
}: {
  sandboxId: string
  // Absent when the Sandbox has no Workspace to publish from.
  workspaceId?: string
}) {
  const [path, setPath] = useState('')
  const queryClient = useQueryClient()
  const inputRef = useRef<HTMLInputElement>(null)

  const listing = useQuery({
    queryKey: ['files', sandboxId, path],
    queryFn: () => api<{ items: FileEntry[] }>('GET', filesPath(sandboxId, path)),
  })

  const upload = useMutation({
    mutationFn: (file: File) =>
      apiRaw('PUT', fileContentPath(sandboxId, joinPath(path, file.name)), file),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['files', sandboxId] })
    },
  })

  // Publishing walks the Workspace on its Node; the version starts pending
  // and moves the Resource's current pointer once ready (api.md §3).
  const publish = useMutation({
    mutationFn: ({ target, resource }: { target: string; resource: string }) =>
      api<ResourceVersion>(
        'POST',
        `/workspaces/${workspaceId}/publish`,
        { path: target || '.', resource },
        { 'Idempotency-Key': crypto.randomUUID() },
      ),
  })
  // The path being published and the Resource name typed for it.
  const [publishing, setPublishing] = useState<{ target: string; resource: string } | null>(null)
  const startPublish = (target: string, name: string) => {
    publish.reset()
    setPublishing({ target, resource: name })
  }

  return (
    <Card>
      <CardHeader className="flex-row items-start justify-between gap-3">
        <CardTitle>{m.files_title()}</CardTitle>
        <div className="flex gap-2">
          {workspaceId && (
            <Button
              variant="outline"
              size="sm"
              disabled={publish.isPending}
              data-testid="files-publish-dir"
              onClick={() => startPublish(path, path.split('/').pop() ?? '')}
            >
              <PackagePlus data-icon="inline-start" />
              {m.files_publish_dir()}
            </Button>
          )}
          <input
            ref={inputRef}
            type="file"
            aria-label={m.files_upload_aria()}
            className="sr-only"
            disabled={upload.isPending}
            onChange={(event) => {
              const file = event.target.files?.[0]
              if (file) upload.mutate(file)
              // Allow uploading the same name again.
              event.target.value = ''
            }}
          />
          <Button
            variant="outline"
            size="sm"
            disabled={upload.isPending}
            onClick={() => inputRef.current?.click()}
          >
            <Upload data-icon="inline-start" />
            {upload.isPending ? m.files_uploading() : m.files_upload()}
          </Button>
        </div>
      </CardHeader>
      <CardContent className="flex flex-col gap-3">
        <div className="flex items-center gap-2">
          <Button
            variant="ghost"
            size="xs"
            disabled={!path}
            onClick={() => setPath(parentPath(path))}
          >
            <ArrowUp data-icon="inline-start" />
            {m.files_up()}
          </Button>
          <span className="truncate font-mono text-xs text-muted-foreground">/{path}</span>
        </div>

        {listing.isPending && (
          <div className="flex items-center gap-2 text-sm text-muted-foreground">
            <Spinner />
            {m.common_loading()}
          </div>
        )}

        {publish.data && publish.variables && (
          <PublishStatus version={publish.data} name={publish.variables.resource} />
        )}

        <Sheet open={publishing !== null} onOpenChange={(open) => !open && setPublishing(null)}>
          <SheetContent>
            <SheetHeader>
              <SheetTitle>{m.files_publish_title()}</SheetTitle>
              <SheetDescription>
                {m.files_publish_description({ path: `/${publishing?.target ?? ''}` })}
              </SheetDescription>
            </SheetHeader>
            <form
              id="publish-form"
              className="flex-1 overflow-y-auto px-4"
              onSubmit={(event) => {
                event.preventDefault()
                const resource = publishing?.resource.trim()
                if (!publishing || !resource) return
                publish.mutate({ target: publishing.target, resource })
                setPublishing(null)
              }}
            >
              <Field>
                <FieldLabel htmlFor="publish-resource">{m.files_publish_name()}</FieldLabel>
                <Input
                  id="publish-resource"
                  autoFocus
                  autoComplete="off"
                  value={publishing?.resource ?? ''}
                  onChange={(event) =>
                    setPublishing((current) => current && { ...current, resource: event.target.value })
                  }
                />
              </Field>
            </form>
            <SheetFooter className="flex-row justify-end">
              <Button type="button" variant="ghost" onClick={() => setPublishing(null)}>
                {m.common_cancel()}
              </Button>
              <Button
                type="submit"
                form="publish-form"
                disabled={!publishing?.resource.trim() || publish.isPending}
              >
                <PackagePlus data-icon="inline-start" />
                {m.files_publish()}
              </Button>
            </SheetFooter>
          </SheetContent>
        </Sheet>

        {listing.data?.items.length === 0 && (
          <Empty className="border py-6">
            <EmptyHeader>
              <EmptyDescription>{m.files_empty()}</EmptyDescription>
            </EmptyHeader>
          </Empty>
        )}
        {listing.data && listing.data.items.length > 0 && (
          <ItemGroup className="gap-1">
            {listing.data.items.map((entry) => {
              const entryPath = entry.path || joinPath(path, entry.name)
              // Only files have a content download; directories are archived,
              // and the API rejects content requests for symlinks/other types.
              const href =
                entry.type === 'file'
                  ? downloadHref('file', sandboxId, entryPath)
                  : entry.type === 'dir'
                    ? downloadHref('dir', sandboxId, entryPath)
                    : null
              return (
                <Item
                  key={entryPath}
                  role="listitem"
                  variant="outline"
                  size="sm"
                  data-testid={`file-${entry.name}`}
                >
                  <ItemMedia variant="icon" className="text-muted-foreground">
                    {entry.type === 'dir' ? <Folder /> : <File />}
                  </ItemMedia>
                  <ItemContent className="min-w-0">
                    <ItemTitle>
                      {entry.type === 'dir' ? (
                        <button
                          type="button"
                          className="truncate text-left hover:underline"
                          onClick={() => setPath(entryPath)}
                        >
                          {entry.name}/
                        </button>
                      ) : (
                        <span className="truncate">{entry.name}</span>
                      )}
                    </ItemTitle>
                    <ItemDescription className="text-xs">
                      {entry.type === 'dir' ? m.files_directory() : formatBytes(entry.size)} ·{' '}
                      {formatLocalDateTime(entry.mod_time)}
                    </ItemDescription>
                  </ItemContent>
                  <ItemActions>
                    {workspaceId && (entry.type === 'file' || entry.type === 'dir') && (
                      <Button
                        variant="ghost"
                        size="xs"
                        disabled={publish.isPending}
                        data-testid={`file-publish-${entry.name}`}
                        onClick={() => startPublish(entryPath, entry.name)}
                      >
                        <PackagePlus data-icon="inline-start" />
                        {m.files_publish()}
                      </Button>
                    )}
                    {href && (
                      <a
                        href={href}
                        download={entry.name}
                        className={cn(buttonVariants({ variant: 'ghost', size: 'xs' }))}
                      >
                        <Download data-icon="inline-start" />
                        {m.files_download()}
                      </a>
                    )}
                  </ItemActions>
                </Item>
              )
            })}
          </ItemGroup>
        )}
      </CardContent>
    </Card>
  )
}

function PublishStatus({ version: accepted, name }: { version: ResourceVersion; name: string }) {
  const version = useQuery({
    queryKey: ['resource-version', accepted.resource_id, accepted.id],
    queryFn: () =>
      api<ResourceVersion>('GET', `/resources/${accepted.resource_id}/versions/${accepted.id}`),
    initialData: accepted,
    refetchInterval: (query) => resourceVersionPollInterval(query.state.data?.state),
  })
  const current = version.data
  return (
    <p className="flex flex-wrap items-center gap-2 text-xs" data-testid="publish-status">
      <span className="text-muted-foreground">{m.files_publish_status({ name })}</span>
      <StatusBadge state={current.state} data-testid="publish-state" />
      {current.cause && <span className="text-muted-foreground">{causeLabel(current.cause)}</span>}
      <Link
        to="/resources/$id"
        params={{ id: current.resource_id }}
        className="text-foreground hover:underline"
      >
        {m.files_publish_open()}
      </Link>
    </p>
  )
}
