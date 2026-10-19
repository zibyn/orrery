import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { createFileRoute } from '@tanstack/react-router'
import { GitBranch, Link2, Unlink } from 'lucide-react'

import { api, type GitLink, type GitProvider, type ListResponse } from '@/api'
import { EmptyState } from '@/components/empty-state'
import { ListCard, ListRow } from '@/components/list'
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
    <div className="flex flex-col gap-6">
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
        <ListCard>
          {providers.data.items.map((provider) => (
            <ListRow
              key={provider.id}
              icon={<GitBranch />}
              title={<span className="truncate text-sm font-medium">{provider.id}</span>}
              description={
                <>
                  {provider.type} · {provider.host}
                </>
              }
              trailing={
                linked.has(provider.id) ? (
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
                )
              }
            />
          ))}
        </ListCard>
      )}
    </div>
  )
}
