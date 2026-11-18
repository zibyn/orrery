import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { createFileRoute, Link, useNavigate } from '@tanstack/react-router'
import { Database, Plus, Trash } from 'lucide-react'
import { useState } from 'react'

import { api, listAll, type Volume } from '@/api'
import { PageHeader } from '@/components/page-header'
import { StatusBadge } from '@/components/status-badge'
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
import { Button } from '@/components/ui/button'
import { Empty, EmptyDescription, EmptyHeader, EmptyMedia, EmptyTitle } from '@/components/ui/empty'
import { Field, FieldDescription, FieldLabel } from '@/components/ui/field'
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
  SheetFooter,
  SheetHeader,
  SheetTitle,
} from '@/components/ui/sheet'
import { Spinner } from '@/components/ui/spinner'
import { useLocale } from '@/lib/i18n'
import { formatTimestamp } from '@/lib/tokens'
import { m } from '@/paraglide/messages'

export const Route = createFileRoute('/_authed/volumes/')({
  component: VolumesPage,
})

function VolumesPage() {
  useLocale() // re-render on a language switch (see lib/i18n.tsx)
  const navigate = useNavigate()
  const queryClient = useQueryClient()
  const [creating, setCreating] = useState<{ name: string; key: string } | null>(null)
  const [confirming, setConfirming] = useState<string | null>(null)
  const volumes = useQuery({
    queryKey: ['volumes'],
    queryFn: () => listAll<Volume>('/volumes'),
  })
  const create = useMutation({
    // One key per opened sheet: a retried submit replays instead of creating twice.
    mutationFn: ({ name, key }: { name: string; key: string }) =>
      api<Volume>('POST', '/volumes', { name }, { 'Idempotency-Key': key }),
    onSuccess: async (volume) => {
      setCreating(null)
      await queryClient.invalidateQueries({ queryKey: ['volumes'] })
      void navigate({ to: '/volumes/$id', params: { id: volume.id } })
    },
  })

  const remove = useMutation({
    mutationFn: (id: string) => api<Volume>('DELETE', `/volumes/${id}`),
    onSettled: () => queryClient.invalidateQueries({ queryKey: ['volumes'] }),
  })

  const live = volumes.data?.filter((volume) => volume.state !== 'deleted')
  const count = live?.length

  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        title={m.volumes_title()}
        description={m.volumes_description()}
        meta={
          count === undefined
            ? undefined
            : count === 1
              ? m.volumes_count_one({ count })
              : m.volumes_count_other({ count })
        }
        actions={
          <Button onClick={() => setCreating({ name: '', key: crypto.randomUUID() })}>
            <Plus data-icon="inline-start" />
            {m.volumes_new()}
          </Button>
        }
      />

      <Sheet open={creating !== null} onOpenChange={(open) => !open && setCreating(null)}>
        <SheetContent>
          <SheetHeader>
            <SheetTitle>{m.volumes_new()}</SheetTitle>
          </SheetHeader>
          <form
            id="volume-form"
            className="flex-1 overflow-y-auto px-4"
            onSubmit={(event) => {
              event.preventDefault()
              const name = creating?.name.trim()
              if (creating && name) create.mutate({ name, key: creating.key })
            }}
          >
            <Field>
              <FieldLabel htmlFor="volume-name">{m.common_name()}</FieldLabel>
              <Input
                id="volume-name"
                autoFocus
                autoComplete="off"
                placeholder="my-data"
                value={creating?.name ?? ''}
                onChange={(event) =>
                  setCreating((current) => current && { ...current, name: event.target.value })
                }
              />
              <FieldDescription>{m.volumes_new_description()}</FieldDescription>
            </Field>
          </form>
          <SheetFooter className="flex-row justify-end">
            <Button type="button" variant="ghost" onClick={() => setCreating(null)}>
              {m.common_cancel()}
            </Button>
            <Button
              type="submit"
              form="volume-form"
              disabled={!creating?.name.trim() || create.isPending}
            >
              {m.common_create()}
            </Button>
          </SheetFooter>
        </SheetContent>
      </Sheet>

      {volumes.isPending && (
        <div className="flex items-center gap-2 py-8 text-sm text-muted-foreground">
          <Spinner />
          {m.common_loading()}
        </div>
      )}

      {live?.length === 0 && (
        <Empty className="border">
          <EmptyHeader>
            <EmptyMedia variant="icon">
              <Database />
            </EmptyMedia>
            <EmptyTitle>{m.volumes_empty_title()}</EmptyTitle>
            <EmptyDescription>{m.volumes_empty_description()}</EmptyDescription>
          </EmptyHeader>
        </Empty>
      )}

      {live && live.length > 0 && (
        <ItemGroup>
          {live.map((volume) => (
            <Item key={volume.id} variant="outline" role="listitem" data-testid={`volume-${volume.name}`}>
              <ItemMedia variant="icon">
                <Database />
              </ItemMedia>
              <ItemContent>
                <ItemTitle>
                  <Link to="/volumes/$id" params={{ id: volume.id }} className="hover:underline">
                    {volume.name}
                  </Link>
                </ItemTitle>
                <ItemDescription>
                  {m.common_created({ time: formatTimestamp(volume.created_at) })}
                  {volume.held_by && (
                    <>
                      {' '}
                      · {m.volume_held_by()}{' '}
                      <span className="font-mono">{volume.held_by.slice(0, 8)}</span>
                    </>
                  )}
                </ItemDescription>
              </ItemContent>
              <ItemActions>
                <StatusBadge state={volume.state} />
                <AlertDialog
                  open={confirming === volume.id}
                  onOpenChange={(next) => setConfirming(next ? volume.id : null)}
                >
                  {/* A disabled button shows no tooltip, so the reason sits on a wrapper. */}
                  <span title={volume.held_by ? m.volume_delete_held() : undefined}>
                    <AlertDialogTrigger
                      render={
                        <Button
                          variant="outline"
                          size="sm"
                          disabled={remove.isPending || volume.held_by !== null}
                          aria-description={volume.held_by ? m.volume_delete_held() : undefined}
                        />
                      }
                    >
                      <Trash data-icon="inline-start" />
                      {m.common_delete()}
                    </AlertDialogTrigger>
                  </span>
                  <AlertDialogContent>
                    <AlertDialogHeader>
                      <AlertDialogTitle>{m.volume_delete_confirm({ name: volume.name })}</AlertDialogTitle>
                      <AlertDialogDescription>{m.common_irreversible()}</AlertDialogDescription>
                    </AlertDialogHeader>
                    <AlertDialogFooter>
                      <AlertDialogCancel>{m.common_cancel()}</AlertDialogCancel>
                      <AlertDialogAction
                        variant="destructive"
                        data-testid="confirm-action"
                        onClick={() => {
                          setConfirming(null)
                          remove.mutate(volume.id)
                        }}
                      >
                        {m.common_delete()}
                      </AlertDialogAction>
                    </AlertDialogFooter>
                  </AlertDialogContent>
                </AlertDialog>
              </ItemActions>
            </Item>
          ))}
        </ItemGroup>
      )}
    </div>
  )
}
