import { useState } from 'react'

import { api, type PortTicket } from '@/api'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'

export function SandboxPorts({ sandboxId }: { sandboxId: string }) {
  const [port, setPort] = useState('')
  const [error, setError] = useState('')
  const [locked, setLocked] = useState(false)
  const [opening, setOpening] = useState(false)

  async function open() {
    const value = Number(port)
    if (!Number.isInteger(value) || value < 1 || value > 65535) {
      setError('Enter a port between 1 and 65535.')
      setLocked(true)
      return
    }
    setOpening(true)
    setError('')
    try {
      const ticket = await api<PortTicket>('POST', `/sandboxes/${sandboxId}/ports/${value}/ticket`)
      window.open(ticket.url, '_blank', 'noopener')
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Could not open the port')
      setLocked(true)
    } finally {
      setOpening(false)
    }
  }

  return (
    <section className="flex flex-col gap-3 rounded-md border border-border p-4 text-sm">
      <h2 className="font-semibold">Ports</h2>
      <div className="flex items-end gap-2">
        <div className="flex flex-col gap-2">
          <Label htmlFor="sandbox-port">Port</Label>
          <Input
            id="sandbox-port"
            type="number"
            min="1"
            max="65535"
            value={port}
            onChange={(event) => {
              setPort(event.target.value)
              setError('')
              setLocked(false)
            }}
            onKeyDown={(event) => {
              if (event.key === 'Enter' && !locked) {
                event.preventDefault()
                void open()
              }
            }}
          />
        </div>
        <Button type="button" disabled={locked || opening || !port} onClick={() => void open()}>
          Open
        </Button>
      </div>
      <p className="text-xs text-muted-foreground">The ticket is valid for one minute.</p>
      {error && (
        <p className="text-destructive" role="alert">
          {error}
        </p>
      )}
    </section>
  )
}
