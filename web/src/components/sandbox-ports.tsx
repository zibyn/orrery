import { ExternalLink } from 'lucide-react'
import { useState } from 'react'

import { api, type PortTicket } from '@/api'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
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
    setError('')
    // The ticket request is async, so open the window now while the click is
    // still a user gesture (a later window.open can be popup-blocked) and
    // navigate it to the ticket URL once the request returns.
    const popup = window.open('', '_blank')
    if (!popup) {
      setError('The browser blocked the new tab. Allow pop-ups and try again.')
      setLocked(true)
      return
    }
    popup.opener = null
    setOpening(true)
    try {
      const ticket = await api<PortTicket>('POST', `/sandboxes/${sandboxId}/ports/${value}/ticket`)
      popup.location.href = ticket.url
    } catch (err) {
      popup.close()
      setError(err instanceof Error ? err.message : 'Could not open the port')
      setLocked(true)
    } finally {
      setOpening(false)
    }
  }

  return (
    <Card>
      <CardHeader>
        <CardTitle>Ports</CardTitle>
        <CardDescription>
          Open a forwarded port in a new tab. The ticket is valid for one minute.
        </CardDescription>
      </CardHeader>
      <CardContent className="flex flex-col gap-3">
        <div className="flex items-end gap-2">
          <div className="flex w-36 flex-col gap-1.5">
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
            <ExternalLink />
            Open
          </Button>
        </div>
        {error && (
          <p className="text-sm text-destructive" role="alert">
            {error}
          </p>
        )}
      </CardContent>
    </Card>
  )
}
