import { useMutation } from '@tanstack/react-query'
import { createFileRoute } from '@tanstack/react-router'
import { CircleCheck, Terminal } from 'lucide-react'
import { useState } from 'react'

import { api } from '@/api'
import { PageHeader } from '@/components/page-header'
import { Button } from '@/components/ui/button'
import { Card, CardContent } from '@/components/ui/card'
import { Field, FieldDescription, FieldLabel } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { Item, ItemContent, ItemDescription, ItemMedia, ItemTitle } from '@/components/ui/item'
import { useLocale } from '@/lib/i18n'
import { m } from '@/paraglide/messages'

// orrery login lands here with ?code= (ADR-0033). Nothing is created until
// the User clicks Approve, so opening or refreshing the page is harmless; the
// token itself goes straight to the polling CLI and never shows here.
export const Route = createFileRoute('/_authed/cli-auth')({
  validateSearch: (search: Record<string, unknown>): { code?: string } => ({
    code: typeof search.code === 'string' ? search.code : undefined,
  }),
  component: CliAuthPage,
})

function CliAuthPage() {
  useLocale() // re-render on a language switch (see lib/i18n.tsx)
  const { code: fromLink } = Route.useSearch()
  const [code, setCode] = useState(fromLink ?? '')
  const approve = useMutation({
    mutationFn: () => api<void>('POST', '/cli-logins/approve', { user_code: code }),
  })

  if (approve.isSuccess) {
    return (
      <div className="mx-auto flex max-w-xl flex-col gap-3 py-6">
        <PageHeader title={m.cli_title()} />
        <Item variant="outline">
          <ItemMedia variant="icon" className="text-success">
            <CircleCheck />
          </ItemMedia>
          <ItemContent>
            <ItemTitle>{m.cli_approved()}</ItemTitle>
            <ItemDescription>{m.cli_approved_hint()}</ItemDescription>
          </ItemContent>
        </Item>
      </div>
    )
  }

  return (
    <div className="mx-auto flex max-w-xl flex-col gap-6 py-6">
      <PageHeader
        above={
          <span className="inline-flex items-center gap-1 text-xs text-muted-foreground">
            <Terminal className="size-3.5" />
            orrery login
          </span>
        }
        title={m.cli_title()}
        description={m.cli_description()}
      />
      <Card>
        <CardContent className="py-5">
          <form
            className="flex flex-col gap-4"
            onSubmit={(e) => {
              e.preventDefault()
              approve.mutate()
            }}
          >
            <Field>
              <FieldLabel htmlFor="cli-code">{m.cli_code()}</FieldLabel>
              <Input
                id="cli-code"
                value={code}
                onChange={(e) => setCode(e.target.value)}
                autoComplete="off"
                spellCheck={false}
                placeholder="XXXX-XXXX"
                className="font-mono text-lg tracking-widest uppercase"
              />
              <FieldDescription>{m.cli_code_hint()}</FieldDescription>
            </Field>
            <Button type="submit" disabled={code.trim() === '' || approve.isPending}>
              {m.cli_approve()}
            </Button>
          </form>
        </CardContent>
      </Card>
    </div>
  )
}
