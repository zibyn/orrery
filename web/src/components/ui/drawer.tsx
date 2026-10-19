import { Drawer as DrawerPrimitive } from '@base-ui/react/drawer'
import { X } from 'lucide-react'
import type * as React from 'react'

import { m } from '@/paraglide/messages'
import { cn } from '@/lib/utils'

// A right-hand sheet for forms that used to sit inline on a list page. The
// form keeps its own submit button; pass it in `footer` with the form's id.
export function Drawer({
  open,
  onOpenChange,
  title,
  description,
  footer,
  children,
  className,
}: {
  open: boolean
  onOpenChange: (open: boolean) => void
  title: React.ReactNode
  description?: React.ReactNode
  footer?: React.ReactNode
  children: React.ReactNode
  className?: string
}) {
  return (
    <DrawerPrimitive.Root open={open} onOpenChange={onOpenChange} swipeDirection="right">
      <DrawerPrimitive.Portal>
        <DrawerPrimitive.Backdrop className="fixed inset-0 z-40 bg-black/30 transition-opacity duration-200 data-[starting-style]:opacity-0 data-[ending-style]:opacity-0" />
        <DrawerPrimitive.Viewport className="fixed inset-0 z-50 flex justify-end">
          <DrawerPrimitive.Popup
            className={cn(
              'flex h-full w-full max-w-md flex-col border-l border-border bg-card text-card-foreground shadow-xl outline-none',
              '[transform:translateX(var(--drawer-swipe-movement-x))] transition-transform duration-300 ease-out',
              'data-[starting-style]:translate-x-full data-[ending-style]:translate-x-full',
              className,
            )}
          >
            <div className="flex items-start gap-3 border-b border-border px-5 py-4">
              <div className="min-w-0 flex-1">
                <DrawerPrimitive.Title className="text-base font-semibold tracking-tight">
                  {title}
                </DrawerPrimitive.Title>
                {description && (
                  <DrawerPrimitive.Description className="mt-0.5 text-xs text-muted-foreground">
                    {description}
                  </DrawerPrimitive.Description>
                )}
              </div>
              <DrawerPrimitive.Close
                aria-label={m.common_close()}
                className="rounded-lg p-1.5 text-muted-foreground transition-colors hover:bg-muted hover:text-foreground focus-visible:ring-3 focus-visible:ring-ring/50 focus-visible:outline-none"
              >
                <X className="size-4" />
              </DrawerPrimitive.Close>
            </div>

            <div className="flex-1 overflow-y-auto px-5 py-4">{children}</div>

            {footer && (
              <div className="flex items-center justify-end gap-2 border-t border-border px-5 py-3">
                {footer}
              </div>
            )}
          </DrawerPrimitive.Popup>
        </DrawerPrimitive.Viewport>
      </DrawerPrimitive.Portal>
    </DrawerPrimitive.Root>
  )
}
