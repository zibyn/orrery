import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { createFileRoute } from '@tanstack/react-router'
import { GitBranch, Link2, Unlink } from 'lucide-react'

import { api, type GitLink, type GitProvider, type ListResponse } from '@/api'
import { EmptyState } from '@/components/empty-state'
import { PageHeader } from '@/components/page-header'
import { Problem } from '@/components/problem'
import { Button, buttonVariants } from '@/components/ui/button'
import { useLocale } from '@/lib/i18n'
import { m } from '@/paraglide/messages'

// The console lists the deployment's Git providers and lets the caller link or
// unlink one (api.md §8). The provider's authorization page starts the OAuth
// flow; the callback returns here.
export const Route = createFileRoute('/_authed/git')({
  component: GitPage,
})

function GitPage() {
  useLocale() // re-render on a language switch (see lib/i18n.tsx)
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
    <div className="flex max-w-3xl flex-col gap-6">
      <PageHeader title={m.git_title()} description={m.git_description()} />

      {(providers.isPending || links.isPending) && (
        <p className="py-8 text-sm text-muted-foreground">{m.git_loading()}</p>
      )}
      {(providers.error ?? links.error) && (
        <p className="text-sm text-destructive" role="alert">
          {m.git_load_error()}: <Problem error={providers.error ?? links.error} />
        </p>
      )}
      {unlink.error && (
        <p className="text-sm text-destructive" role="alert">
          {m.git_unlink_error()}: <Problem error={unlink.error} />
        </p>
      )}

      {providers.data?.items.length === 0 && (
        <EmptyState
          icon={<GitBranch />}
          title={m.git_empty_title()}
          description={m.git_empty_description()}
        />
      )}

      {providers.data && providers.data.items.length > 0 && (
        <div className="divide-y divide-border overflow-hidden rounded-xl border border-border bg-card">
          {providers.data.items.map((provider) => (
            <div key={provider.id} className="flex items-center gap-3 px-4 py-3">
              <span className="flex size-8 shrink-0 items-center justify-center rounded-lg border border-border bg-muted/50 text-muted-foreground">
                <GitBranch className="size-4" />
              </span>
              <div className="min-w-0 flex-1">
                <p className="truncate text-sm font-medium">{provider.id}</p>
                <p className="truncate text-xs text-muted-foreground">
                  {provider.type} · {provider.host}
                </p>
              </div>
              {linked.has(provider.id) ? (
                <Button
                  variant="outline"
                  size="sm"
                  disabled={unlink.isPending}
                  onClick={() => unlink.mutate(provider.id)}
                >
                  <Unlink />
                  {m.git_unlink()}
                </Button>
              ) : (
                <a
                  className={buttonVariants({ size: 'sm' })}
                  href={`/api/v1/git-providers/${encodeURIComponent(provider.id)}/authorize`}
                >
                  <Link2 />
                  {m.git_link()}
                </a>
              )}
            </div>
          ))}
        </div>
      )}
    </div>
  )
}
