import { useState } from 'react'

import { Button } from '@/components/ui/button'

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
        className="overflow-x-auto rounded-md border border-border bg-muted p-3 font-mono text-sm"
      >
        {token}
      </pre>
      <div className="flex gap-2">
        <Button onClick={copy}>{copied ? 'Copied' : 'Copy token'}</Button>
        {onDone && (
          <Button variant="outline" onClick={onDone}>
            Done
          </Button>
        )}
      </div>
    </div>
  )
}
