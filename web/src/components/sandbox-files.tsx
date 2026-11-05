import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { ArrowUp, Download, File, Folder, PackagePlus, Upload } from 'lucide-react'
import { useRef, useState } from 'react'

import { api, apiRaw, type FileEntry, type ResourceVersion } from '@/api'
import { Problem } from '@/components/problem'
import { StatusBadge } from '@/components/status-badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Drawer } from '@/components/ui/drawer'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import {
  downloadHref,
  fileContentPath,
  filesPath,
  formatBytes,
  joinPath,
  parentPath,
} from '@/lib/files'
import { causeLabel, messageOf } from '@/lib/labels'
import { resourceVersionPollInterval } from '@/lib/resources'
import { formatLocalDateTime } from '@/lib/tokens'
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
  const [uploadError, setUploadError] = useState('')
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
      setUploadError('')
      queryClient.invalidateQueries({ queryKey: ['files', sandboxId] })
    },
    onError: (err) => setUploadError(messageOf(err)),
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
              <PackagePlus />
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
            <Upload />
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
            <ArrowUp />
            {m.files_up()}
          </Button>
          <span className="truncate font-mono text-xs text-muted-foreground">/{path}</span>
        </div>

        {listing.isPending && (
          <p className="text-muted-foreground">{m.common_loading()}</p>
        )}
        {listing.error && (
          <p className="text-destructive" role="alert">
            <Problem error={listing.error} />
          </p>
        )}
        {uploadError && (
          <p className="whitespace-pre-line text-destructive" role="alert">
            {uploadError}
          </p>
        )}

        {publish.error && (
          <p className="text-destructive" role="alert">
            <Problem error={publish.error} />
          </p>
        )}
        {publish.data && publish.variables && (
          <PublishStatus version={publish.data} name={publish.variables.resource} />
        )}

        <Drawer
          open={publishing !== null}
          onOpenChange={(open) => !open && setPublishing(null)}
          title={m.files_publish_title()}
          description={m.files_publish_description({ path: `/${publishing?.target ?? ''}` })}
          footer={
            <>
              <Button type="button" variant="ghost" onClick={() => setPublishing(null)}>
                {m.common_cancel()}
              </Button>
              <Button
                type="submit"
                form="publish-form"
                disabled={!publishing?.resource.trim() || publish.isPending}
              >
                <PackagePlus />
                {m.files_publish()}
              </Button>
            </>
          }
        >
          <form
            id="publish-form"
            className="flex flex-col gap-1.5"
            onSubmit={(event) => {
              event.preventDefault()
              const resource = publishing?.resource.trim()
              if (!publishing || !resource) return
              publish.mutate({ target: publishing.target, resource })
              setPublishing(null)
            }}
          >
            <Label htmlFor="publish-resource">{m.files_publish_name()}</Label>
            <Input
              id="publish-resource"
              autoFocus
              autoComplete="off"
              value={publishing?.resource ?? ''}
              onChange={(event) =>
                setPublishing((current) => current && { ...current, resource: event.target.value })
              }
            />
          </form>
        </Drawer>

        {listing.data && (
          <ul className="divide-y divide-border overflow-hidden rounded-lg border border-border">
            {listing.data.items.length === 0 && (
              <li className="px-3 py-6 text-center text-xs text-muted-foreground">
                {m.files_empty()}
              </li>
            )}
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
                <li
                  key={entryPath}
                  className="flex min-w-0 items-center gap-2.5 px-3 py-2"
                  data-testid={`file-${entry.name}`}
                >
                  {entry.type === 'dir' ? (
                    <Folder className="size-4 shrink-0 text-muted-foreground" />
                  ) : (
                    <File className="size-4 shrink-0 text-muted-foreground" />
                  )}
                  <div className="flex min-w-0 flex-1 flex-col">
                    {entry.type === 'dir' ? (
                      <button
                        type="button"
                        className="self-start truncate text-left text-sm font-medium hover:underline"
                        onClick={() => setPath(entryPath)}
                      >
                        {entry.name}/
                      </button>
                    ) : (
                      <span className="truncate text-sm font-medium">{entry.name}</span>
                    )}
                    <span className="text-xs text-muted-foreground">
                      {entry.type === 'dir' ? m.files_directory() : formatBytes(entry.size)} ·{' '}
                      {formatLocalDateTime(entry.mod_time)}
                    </span>
                  </div>
                  {workspaceId && (entry.type === 'file' || entry.type === 'dir') && (
                    <button
                      type="button"
                      disabled={publish.isPending}
                      data-testid={`file-publish-${entry.name}`}
                      onClick={() => startPublish(entryPath, entry.name)}
                      className="inline-flex shrink-0 items-center gap-1 rounded-md px-1.5 py-1 text-xs text-muted-foreground transition-colors hover:bg-muted hover:text-foreground disabled:opacity-50"
                    >
                      <PackagePlus className="size-3.5" />
                      {m.files_publish()}
                    </button>
                  )}
                  {href && (
                    <a
                      href={href}
                      download={entry.name}
                      className="inline-flex shrink-0 items-center gap-1 rounded-md px-1.5 py-1 text-xs text-muted-foreground transition-colors hover:bg-muted hover:text-foreground"
                    >
                      <Download className="size-3.5" />
                      {m.files_download()}
                    </a>
                  )}
                </li>
              )
            })}
          </ul>
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
      {version.error && (
        <span className="text-destructive" role="alert">
          <Problem error={version.error} />
        </span>
      )}
    </p>
  )
}
