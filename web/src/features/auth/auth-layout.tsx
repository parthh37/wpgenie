import type { ReactNode } from "react"
import { ArchiveIcon, BoxIcon, LockIcon, ShieldIcon } from "lucide-react"
import { Logo } from "@/components/app/logo"
import { IconTile } from "@/components/app/icon-tile"

// The signed-out screens: a plain card on the canvas, and what WPGenie
// does beside it.
export function AuthLayout({ children, aside = true }: { children: ReactNode; aside?: boolean }) {
  return (
    <div className="flex min-h-svh items-center justify-center p-4 sm:p-8">
      <div className="grid w-full max-w-4xl items-center gap-10 md:grid-cols-[minmax(0,26rem)_1fr]">
        <div className="rounded-lg bg-card p-7 card-shadow sm:p-8">
          <div className="mb-6 flex items-center gap-2.5 text-lg font-semibold">
            <Logo className="size-7" /> WPGenie
          </div>
          {children}
        </div>
        {aside && (
          <ul aria-label="What WPGenie does" className="hidden list-none flex-col gap-6 p-0 md:flex">
            <AsideItem icon={BoxIcon} title="Every site in its own container" text="Unprivileged, read-only PHP with automatic HTTPS." />
            <AsideItem icon={ShieldIcon} title="Protection in front of WordPress" text="Bot and AI-crawler blocking, WAF, and a CAPTCHA with no third party." />
            <AsideItem icon={ArchiveIcon} title="Backups you can trust" text="Encrypted, deduplicated, restored in one click." />
            <AsideItem icon={LockIcon} title="Yours, on your server" text="Visitor data never leaves it." />
          </ul>
        )}
      </div>
    </div>
  )
}

function AsideItem({ icon, title, text }: { icon: typeof BoxIcon; title: string; text: string }) {
  return (
    <li className="flex items-start gap-3.5">
      <IconTile icon={icon} size="lg" />
      <div className="flex flex-col gap-0.5">
        <strong className="font-semibold">{title}</strong>
        <span className="text-sm text-muted-foreground">{text}</span>
      </div>
    </li>
  )
}
