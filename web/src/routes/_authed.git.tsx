import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { createFileRoute } from '@tanstack/react-router'
import { GitBranch, Link2, Unlink } from 'lucide-react'
import { useState } from 'react'

import { api, type GitLink, type GitProvider, type ListResponse } from '@/api'
import { PageHeader } from '@/components/page-header'
import { Problem } from '@/components/problem'
import { Alert, AlertDescription } from '@/components/ui/alert'
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
  AlertDialogTrigger,
} from '@/components/ui/alert-dialog'
import { Badge } from '@/components/ui/badge'
import { Button, buttonVariants } from '@/components/ui/button'
import {
  Empty,
  EmptyDescription,
  EmptyHeader,
  EmptyMedia,
  EmptyTitle,
} from '@/components/ui/empty'
import {
  Item,
  ItemActions,
  ItemContent,
  ItemDescription,
  ItemGroup,
  ItemMedia,
  ItemTitle,
} from '@/components/ui/item'
import { Spinner } from '@/components/ui/spinner'
import { useLocale } from '@/lib/i18n'
import { cn } from '@/lib/utils'
import { m } from '@/paraglide/messages'

// The console lists the deployment's Git providers and lets the caller link or
// unlink one (api.md §8). The provider's authorization page starts the OAuth
// flow; the callback returns here.
export const Route = createFileRoute('/_authed/git')({
  component: GitPage,
})

function GitPage() {
  // The provider whose unlink is being confirmed.
  const [confirming, setConfirming] = useState<string | null>(null)
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
        <div className="flex items-center gap-2 py-8 text-sm text-muted-foreground">
          <Spinner />
          {m.git_loading()}
        </div>
      )}
      {(providers.error ?? links.error) && (
        <Alert variant="destructive">
          <AlertDescription>
            {m.git_load_error()}: <Problem error={providers.error ?? links.error} />
          </AlertDescription>
        </Alert>
      )}
      {unlink.error && (
        <Alert variant="destructive">
          <AlertDescription>
            {m.git_unlink_error()}: <Problem error={unlink.error} />
          </AlertDescription>
        </Alert>
      )}

      {providers.data?.items.length === 0 && (
        <Empty className="border">
          <EmptyHeader>
            <EmptyMedia variant="icon">
              <GitBranch />
            </EmptyMedia>
            <EmptyTitle>{m.git_empty_title()}</EmptyTitle>
            <EmptyDescription>{m.git_empty_description()}</EmptyDescription>
          </EmptyHeader>
        </Empty>
      )}

      {providers.data && providers.data.items.length > 0 && (
        <ItemGroup className="gap-2">
          {providers.data.items.map((provider) => (
            <Item key={provider.id} variant="outline" role="listitem">
              <ItemMedia variant="icon">
                <GitBranch />
              </ItemMedia>
              <ItemContent>
                <ItemTitle>{provider.id}</ItemTitle>
                <ItemDescription>
                  {provider.type} · {provider.host}
                </ItemDescription>
              </ItemContent>
              <ItemActions>
                {linked.has(provider.id) ? (
                  <>
                    <Badge variant="success">{m.git_linked()}</Badge>
                    <AlertDialog open={confirming === provider.id} onOpenChange={(next) => setConfirming(next ? provider.id : null)}>
                      <AlertDialogTrigger
                        render={<Button variant="outline" size="sm" disabled={unlink.isPending} />}
                      >
                        <Unlink data-icon="inline-start" />
                        {m.git_unlink()}
                      </AlertDialogTrigger>
                      <AlertDialogContent>
                        <AlertDialogHeader>
                          <AlertDialogTitle>
                            {m.git_unlink_title({ provider: provider.id })}
                          </AlertDialogTitle>
                          <AlertDialogDescription>
                            {m.git_unlink_description({ provider: provider.host })}
                          </AlertDialogDescription>
                        </AlertDialogHeader>
                        <AlertDialogFooter>
                          <AlertDialogCancel>{m.common_cancel()}</AlertDialogCancel>
                          <AlertDialogAction
                            variant="destructive"
                            data-testid="confirm-action"
                            onClick={() => {
                              setConfirming(null)
                              unlink.mutate(provider.id)
                            }}
                          >
                            {m.git_unlink()}
                          </AlertDialogAction>
                        </AlertDialogFooter>
                      </AlertDialogContent>
                    </AlertDialog>
                  </>
                ) : (
                  <a
                    href={`/api/v1/git-providers/${encodeURIComponent(provider.id)}/authorize`}
                    className={cn(buttonVariants({ size: 'sm' }))}
                  >
                    <Link2 data-icon="inline-start" />
                    {m.git_link()}
                  </a>
                )}
              </ItemActions>
            </Item>
          ))}
        </ItemGroup>
      )}
    </div>
  )
}
