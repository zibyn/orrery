import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'

import { api, apiVoid, type FileEntry } from '@/api'
import { Button } from '@/components/ui/button'
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

  const listing = useQuery({
    queryKey: ['files', sandboxId, path],
    queryFn: () => api<{ items: FileEntry[] }>('GET', filesPath(sandboxId, path)),
  })

  const upload = useMutation({
    mutationFn: (file: File) =>
      apiVoid('PUT', fileContentPath(sandboxId, joinPath(path, file.name)), file),
    onSuccess: () => {
      setUploadError('')
      queryClient.invalidateQueries({ queryKey: ['files', sandboxId] })
    },
    onError: (err) => setUploadError(err.message),
  })

  return (
    <section className="flex flex-col gap-3 rounded-md border border-border p-4 text-sm">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <h2 className="font-semibold">Files</h2>
        <input
          type="file"
          aria-label="Upload file"
          className="text-sm"
          disabled={upload.isPending}
          onChange={(event) => {
            const file = event.target.files?.[0]
            if (file) upload.mutate(file)
            // Allow uploading the same name again.
            event.target.value = ''
          }}
        />
      </div>

      <div className="flex items-center gap-2">
        <Button variant="outline" size="sm" disabled={!path} onClick={() => setPath(parentPath(path))}>
          Up
        </Button>
        <span className="font-mono text-xs text-muted-foreground">/{path}</span>
      </div>

      {listing.isPending && <p className="text-muted-foreground">Loading…</p>}
      {listing.error && <p className="text-destructive">{listing.error.message}</p>}
      {uploadError && <p className="text-destructive">{uploadError}</p>}

      {listing.data && (
        <ul className="divide-y divide-border rounded-md border border-border">
          {listing.data.items.length === 0 && (
            <li className="p-3 text-muted-foreground">Empty directory.</li>
          )}
          {listing.data.items.map((entry) => {
            const entryPath = entry.path || joinPath(path, entry.name)
            return (
              <li
                key={entryPath}
                className="flex flex-wrap items-center justify-between gap-2 p-3"
                data-testid={`file-${entry.name}`}
              >
                <div className="flex min-w-0 flex-col">
                  {entry.type === 'dir' ? (
                    <button
                      type="button"
                      className="self-start text-left font-medium hover:underline"
                      onClick={() => setPath(entryPath)}
                    >
                      {entry.name}/
                    </button>
                  ) : (
                    <span className="truncate font-medium">{entry.name}</span>
                  )}
                  <span className="text-xs text-muted-foreground">
                    {entry.type} · {entry.type === 'dir' ? '—' : formatBytes(entry.size)} ·{' '}
                    {new Date(entry.mod_time).toLocaleString()}
                  </span>
                </div>
                <a
                  href={downloadHref(entry.type === 'dir' ? 'dir' : 'file', sandboxId, entryPath)}
                  download={entry.name}
                  className="text-muted-foreground hover:underline"
                >
                  Download
                </a>
              </li>
            )
          })}
        </ul>
      )}
    </section>
  )
}
