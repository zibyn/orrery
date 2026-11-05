import { locales, type Locale } from '@/paraglide/runtime'
import { m } from '@/paraglide/messages'

import { ToggleGroup, ToggleGroupItem } from '@/components/ui/toggle-group'
import { useLocale } from '@/lib/i18n'

const LABELS: Record<Locale, string> = { en: 'English', zh: '中文' }

// Each language is named in itself, so the control is readable before the
// switch (api.md §8).
export function LocaleSwitch({ className }: { className?: string }) {
  const { locale, setLocale } = useLocale()

  return (
    <ToggleGroup
      aria-label={m.locale_switch_label()}
      variant="outline"
      size="sm"
      spacing={0}
      className={className}
      value={[locale]}
      // Pressing the active language again would empty the group; keep it.
      onValueChange={(value) => value[0] && setLocale(value[0] as Locale)}
    >
      {locales.map((item) => (
        <ToggleGroupItem key={item} value={item}>
          {LABELS[item]}
        </ToggleGroupItem>
      ))}
    </ToggleGroup>
  )
}
