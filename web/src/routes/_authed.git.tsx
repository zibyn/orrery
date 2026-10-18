import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { createFileRoute } from '@tanstack/react-router'

import { api, type GitLink, type GitProvider, type ListResponse } from '@/api'
import { Button, buttonVariants } from '@/components/ui/button'

// The console lists the deployment's Git providers and lets the caller link or
// unlink one (api.md §8). The provider's authorization page starts the OAuth
// flow; the callback returns here.
export const Route = createFileRoute('/_authed/git')({
  component: GitPage,
})

function GitPage() {
  const queryClient = useQueryClient()
  const providers = useQuery({
    queryKey: ['git-providers'],
    queryFn: () => api<ListResponse<GitProvider>>('GET', '/git-providers'),
  })
  const links = useQuery({
    queryKey: ['git-links'],
    queryFn: () => api<ListResponse<GitLink>>('GET', '/users/me/git-links'),
  })
  const unlink = useMutation({
    mutationFn: (providerId: string) =>
      api('DELETE', `/users/me/git-links/${encodeURIComponent(providerId)}`),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ['git-links'] }),
  })

  const linked = new Set(links.data?.items.map((link) => link.provider_id))

  return (
    <div className="mx-auto flex min-h-screen max-w-2xl flex-col gap-6 px-4 py-12">
      <div>
        <h1 className="text-2xl font-semibold">Git account links</h1>
        <p className="text-sm text-muted-foreground">
          Link an account so git inside your Sandboxes can clone and push on your behalf.
        </p>
      </div>

      {(providers.isPending || links.isPending) && (
        <p className="text-sm text-muted-foreground">Loading…</p>
      )}
      {(providers.error ?? links.error) && (
        <p className="text-sm text-destructive">
          Could not load Git providers: {(providers.error ?? links.error)?.message}
        </p>
      )}
      {unlink.error && (
        <p className="text-sm text-destructive">Could not unlink: {unlink.error.message}</p>
      )}

      {providers.data?.items.length === 0 && (
        <p className="text-sm text-muted-foreground">This deployment has no Git providers.</p>
      )}

      <ul className="flex flex-col divide-y divide-border rounded-md border border-border">
        {providers.data?.items.map((provider) => (
          <li key={provider.id} className="flex items-center justify-between gap-4 p-4">
            <div>
              <p className="font-medium">{provider.id}</p>
              <p className="text-xs text-muted-foreground">
                {provider.type} · {provider.host}
              </p>
            </div>
            {linked.has(provider.id) ? (
              <Button
                variant="outline"
                disabled={unlink.isPending}
                onClick={() => unlink.mutate(provider.id)}
              >
                Unlink
              </Button>
            ) : (
              <a
                className={buttonVariants()}
                href={`/api/v1/git-providers/${encodeURIComponent(provider.id)}/authorize`}
              >
                Link
              </a>
            )}
          </li>
        ))}
      </ul>
    </div>
  )
}
