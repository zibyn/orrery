import { Check, Copy } from 'lucide-react'
import { useState } from 'react'

import { Button } from '@/components/ui/button'
import { m } from '@/paraglide/messages'

// The one-time plaintext display shared by /cli-auth and the account page
// (api.md §8: the value is shown once and never stored or re-fetched).
export function TokenReveal({
  token,
  hint,
  onDone,
}: {
  token: string
  hint?: string
  onDone?: () => void
}) {
  const [copied, setCopied] = useState(false)

  async function copy() {
    try {
      await navigator.clipboard.writeText(token)
      setCopied(true)
    } catch {
      // Clipboard access can be denied; the token stays visible to select.
    }
  }

  return (
    <div className="flex flex-col gap-3">
      {hint && <p className="text-sm text-muted-foreground">{hint}</p>}
      <pre
        data-testid="token-plaintext"
        className="overflow-x-auto rounded-lg border border-border bg-muted/60 p-3 font-mono text-xs break-all whitespace-pre-wrap"
      >
        {token}
      </pre>
      <div className="flex gap-2">
        <Button onClick={copy} variant={copied ? 'outline' : 'default'}>
          {copied ? <Check /> : <Copy />}
          {copied ? m.token_copied() : m.token_copy()}
        </Button>
        {onDone && (
          <Button variant="outline" onClick={onDone}>
            {m.token_done()}
          </Button>
        )}
      </div>
    </div>
  )
}
