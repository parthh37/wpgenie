import { cn } from "@/lib/utils"

// The WPGenie mark: a sparkle on the brand gradient.
export function Logo({ className }: { className?: string }) {
  return (
    <span aria-hidden className={cn("brand-gradient inline-flex size-7 shrink-0 items-center justify-center rounded-[28%] text-white shadow-sm", className)}>
      <svg viewBox="0 0 24 24" className="size-[70%] fill-current">
        <path d="M12 2.5l2.2 6.3 6.3 2.2-6.3 2.2L12 19.5l-2.2-6.3L3.5 11l6.3-2.2z" />
        <path d="M19 17.5l.8 1.7 1.7.8-1.7.8-.8 1.7-.8-1.7-1.7-.8 1.7-.8z" />
      </svg>
    </span>
  )
}
