import type { ReactNode } from "react"
import { ArchiveIcon, BoxIcon, LockIcon, ShieldIcon } from "lucide-react"
import { Logo } from "@/components/app/logo"
import { IconTile } from "@/components/app/icon-tile"
import { cn } from "@/lib/utils"

// The signed-out screens: a card on the brand's colours, blurred far behind,
// and what WPGenie does beside it.
export function AuthLayout({ children, aside = true }: { children: ReactNode; aside?: boolean }) {
  return (
    <div className="relative isolate flex min-h-svh items-center justify-center p-4 sm:p-8">
      <div
        aria-hidden
        className="fixed inset-0 -z-10 bg-[radial-gradient(40vw_40vw_at_20%_15%,rgba(94,92,230,.35),transparent_70%),radial-gradient(35vw_35vw_at_85%_80%,rgba(191,90,242,.25),transparent_70%),radial-gradient(30vw_30vw_at_70%_10%,rgba(255,55,95,.14),transparent_70%)]"
      />
      <div className={cn("grid w-full items-center gap-10", aside ? "max-w-4xl md:grid-cols-[minmax(0,26rem)_1fr]" : "max-w-[26rem]")}>
        <div className="material rounded-3xl bg-card/85 p-7 shadow-[0_22px_70px_rgba(0,0,0,.25),0_0_0_.5px_rgba(127,127,127,.2)] sm:p-9">
          <div className="mb-6 flex items-center gap-2.5 text-[1.0625rem] font-semibold">
            <Logo className="size-8" /> WPGenie
          </div>
          {children}
        </div>
        {aside && (
          <ul aria-label="What WPGenie does" className="hidden list-none flex-col gap-6 p-0 md:flex">
            <AsideItem icon={BoxIcon} tint="indigo" title="Every site in its own container" text="Unprivileged, read-only PHP with automatic HTTPS." />
            <AsideItem icon={ShieldIcon} tint="green" title="Protection in front of WordPress" text="Bot and AI-crawler blocking, WAF, and a CAPTCHA with no third party." />
            <AsideItem icon={ArchiveIcon} tint="orange" title="Backups you can trust" text="Encrypted, deduplicated, restored in one click." />
            <AsideItem icon={LockIcon} tint="blue" title="Yours, on your server" text="Visitor data never leaves it." />
          </ul>
        )}
      </div>
    </div>
  )
}

function AsideItem({ icon, tint, title, text }: { icon: typeof BoxIcon; tint: "indigo" | "green" | "orange" | "blue"; title: string; text: string }) {
  return (
    <li className="flex items-start gap-3.5">
      <IconTile icon={icon} tint={tint} size="lg" />
      <div className="flex flex-col gap-0.5">
        <strong className="font-semibold">{title}</strong>
        <span className="text-sm text-muted-foreground">{text}</span>
      </div>
    </li>
  )
}
