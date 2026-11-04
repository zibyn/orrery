import { AlertDialog } from '@base-ui/react/alert-dialog'
import { useState, type ReactNode } from 'react'

import { Button } from '@/components/ui/button'
import { m } from '@/paraglide/messages'

// A button that asks before doing something that cannot be undone. The dialog
// names the target and the consequence; the confirm button repeats the action.
export function ConfirmButton({
  title,
  description,
  confirmLabel,
  onConfirm,
  children,
  disabled,
  variant = 'ghost',
  size,
  className,
  testId,
}: {
  title: ReactNode
  description?: ReactNode
  confirmLabel?: ReactNode
  onConfirm: () => void
  children: ReactNode
  disabled?: boolean
  variant?: 'ghost' | 'outline' | 'destructive'
  size?: 'sm' | 'default'
  className?: string
  testId?: string
}) {
  const [open, setOpen] = useState(false)
  return (
    <AlertDialog.Root open={open} onOpenChange={setOpen}>
      <AlertDialog.Trigger
        render={
          <Button
            variant={variant}
            size={size}
            className={className}
            disabled={disabled}
            data-testid={testId}
          />
        }
      >
        {children}
      </AlertDialog.Trigger>
      <AlertDialog.Portal>
        <AlertDialog.Backdrop className="fixed inset-0 z-40 bg-black/30 transition-opacity duration-150 data-[starting-style]:opacity-0 data-[ending-style]:opacity-0" />
        <AlertDialog.Popup className="fixed top-1/2 left-1/2 z-50 w-[calc(100%-2rem)] max-w-md -translate-x-1/2 -translate-y-1/2 rounded-xl border border-border bg-card p-5 text-card-foreground shadow-xl outline-none transition-[opacity,transform] duration-150 data-[ending-style]:scale-95 data-[ending-style]:opacity-0 data-[starting-style]:scale-95 data-[starting-style]:opacity-0">
          <AlertDialog.Title className="text-base font-semibold tracking-tight">
            {title}
          </AlertDialog.Title>
          {description && (
            <AlertDialog.Description className="mt-1.5 text-sm text-muted-foreground">
              {description}
            </AlertDialog.Description>
          )}
          <div className="mt-5 flex justify-end gap-2">
            <AlertDialog.Close render={<Button variant="ghost" />}>
              {m.common_cancel()}
            </AlertDialog.Close>
            <Button
              className="bg-destructive text-white hover:bg-destructive/90"
              data-testid="confirm-action"
              onClick={() => {
                setOpen(false)
                onConfirm()
              }}
            >
              {confirmLabel ?? m.common_delete()}
            </Button>
          </div>
        </AlertDialog.Popup>
      </AlertDialog.Portal>
    </AlertDialog.Root>
  )
}
