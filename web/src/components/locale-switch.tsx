import { locales, type Locale } from '@/paraglide/runtime'
import { m } from '@/paraglide/messages'

import { useLocale } from '@/lib/i18n'
import { cn } from '@/lib/utils'

const LABELS: Record<Locale, string> = { en: 'English', zh: '中文' }

// Each language is named in itself, so the control is readable before the
// switch (api.md §8).
export function LocaleSwitch({ className }: { className?: string }) {
  const { locale, setLocale } = useLocale()

  return (
    <div
      role="group"
      aria-label={m.locale_switch_label()}
      className={cn(
        'inline-flex items-center gap-0.5 rounded-md border border-border bg-muted/40 p-0.5',
        className,
      )}
    >
      {locales.map((item) => (
        <button
          key={item}
          type="button"
          aria-pressed={item === locale}
          onClick={() => setLocale(item)}
          className={cn(
            'rounded px-2 py-1 text-[11px] font-medium transition-colors',
            item === locale
              ? 'bg-background text-foreground shadow-sm'
              : 'text-muted-foreground hover:text-foreground',
          )}
        >
          {LABELS[item]}
        </button>
      ))}
    </div>
  )
}
