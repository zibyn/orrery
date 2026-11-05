import { Check, Copy } from 'lucide-react'
import { useState } from 'react'

import { Button } from '@/components/ui/button'
import { InputGroup, InputGroupAddon, InputGroupButton, InputGroupText } from '@/components/ui/input-group'
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
      {/* Text, not an input value: the token wraps in full and e2e reads its textContent. */}
      <InputGroup className="h-auto">
        <InputGroupText
          data-testid="token-plaintext"
          className="min-w-0 flex-1 px-2.5 py-2 font-mono text-xs break-all text-foreground select-all"
        >
          {token}
        </InputGroupText>
        <InputGroupAddon align="inline-end">
          <InputGroupButton onClick={copy}>
            {copied ? <Check data-icon="inline-start" /> : <Copy data-icon="inline-start" />}
            {copied ? m.token_copied() : m.token_copy()}
          </InputGroupButton>
        </InputGroupAddon>
      </InputGroup>
      {onDone && (
        <div className="flex gap-2">
          <Button variant="outline" onClick={onDone}>
            {m.token_done()}
          </Button>
        </div>
      )}
    </div>
  )
}
