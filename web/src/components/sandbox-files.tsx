import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { ArrowUp, Download, File, Folder, Upload } from 'lucide-react'
import { useRef, useState } from 'react'

import { api, apiRaw, type FileEntry } from '@/api'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import {
  downloadHref,
  fileContentPath,
  filesPath,
  formatBytes,
  joinPath,
  parentPath,
} from '@/lib/files'

export function SandboxFiles({ sandboxId }: { sandboxId: string }) {
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
    onError: (err) => setUploadError(err.message),
  })

  return (
    <Card>
      <CardHeader className="flex-row items-start justify-between gap-3">
        <div className="flex flex-col gap-1">
          <CardTitle>Files</CardTitle>
          <CardDescription className="font-mono">/{path}</CardDescription>
        </div>
        <div>
          <input
            ref={inputRef}
            type="file"
            aria-label="Upload file"
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
            {upload.isPending ? 'Uploading…' : 'Upload'}
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
            Up
          </Button>
          <span className="truncate font-mono text-xs text-muted-foreground">/{path}</span>
        </div>

        {listing.isPending && <p className="text-muted-foreground">Loading…</p>}
        {listing.error && (
          <p className="text-destructive" role="alert">
            {listing.error.message}
          </p>
        )}
        {uploadError && (
          <p className="text-destructive" role="alert">
            {uploadError}
          </p>
        )}

        {listing.data && (
          <ul className="divide-y divide-border overflow-hidden rounded-lg border border-border">
            {listing.data.items.length === 0 && (
              <li className="px-3 py-6 text-center text-xs text-muted-foreground">
                Empty directory.
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
                      {entry.type === 'dir' ? 'directory' : formatBytes(entry.size)} ·{' '}
                      {new Date(entry.mod_time).toLocaleString()}
                    </span>
                  </div>
                  {href && (
                    <a
                      href={href}
                      download={entry.name}
                      className="inline-flex shrink-0 items-center gap-1 rounded-md px-1.5 py-1 text-xs text-muted-foreground transition-colors hover:bg-muted hover:text-foreground"
                    >
                      <Download className="size-3.5" />
                      Download
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
