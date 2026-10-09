import { Button as ButtonPrimitive } from "@base-ui/react/button"
import { cva, type VariantProps } from "class-variance-authority"
import { cn } from "@/lib/utils"

// Kumo's buttons: 36px, 8px corners, a 1px ring, shadow-xs.
const buttonVariants = cva(
  "group/button inline-flex shrink-0 cursor-pointer items-center justify-center rounded-lg border border-transparent text-base font-medium whitespace-nowrap transition-[color,background-color,box-shadow,opacity] outline-none select-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-1 focus-visible:ring-offset-background disabled:pointer-events-none disabled:opacity-50 aria-invalid:ring-2 aria-invalid:ring-destructive/40 [&_svg]:pointer-events-none [&_svg]:shrink-0 [&_svg:not([class*='size-'])]:size-4",
  {
    variants: {
      variant: {
        // Kumo's primary: Cloudflare blue in a ring 10% darker.
        default:
          "bg-primary text-primary-foreground shadow-xs ring-1 ring-[color-mix(in_oklch,var(--primary),black_10%)] hover:bg-primary-hover",
        // Kumo's secondary: the card's colour in a line-coloured ring.
        outline: "bg-card text-foreground shadow-xs ring-1 ring-border hover:bg-accent aria-expanded:bg-accent",
        secondary: "bg-card text-foreground shadow-xs ring-1 ring-border hover:bg-accent aria-expanded:bg-accent",
        ghost: "text-foreground hover:bg-accent aria-expanded:bg-accent",
        // The panel's usual secondary button (named in the older, Apple
        // styled theme): Kumo's secondary.
        tinted: "bg-card text-foreground shadow-xs ring-1 ring-border hover:bg-accent aria-expanded:bg-accent",
        "destructive-solid":
          "bg-danger-fill text-white shadow-xs ring-1 ring-[color-mix(in_oklch,var(--danger-fill),black_10%)] hover:bg-[color-mix(in_oklch,var(--danger-fill),black_10%)] focus-visible:ring-destructive/60",
        // Kumo's secondary-destructive: red words on the secondary button.
        destructive:
          "bg-card text-destructive shadow-xs ring-1 ring-border hover:bg-destructive/5 hover:ring-destructive/30 focus-visible:ring-destructive/60",
        link: "text-link underline-offset-4 hover:underline",
      },
      size: {
        default: "h-9 gap-1.5 px-3 has-data-[icon=inline-end]:pr-2.5 has-data-[icon=inline-start]:pl-2.5",
        xs: "h-6 gap-1 rounded-md px-2 text-xs has-data-[icon=inline-end]:pr-1.5 has-data-[icon=inline-start]:pl-1.5 [&_svg:not([class*='size-'])]:size-3",
        sm: "h-8 gap-1 rounded-md px-2.5 text-sm has-data-[icon=inline-end]:pr-2 has-data-[icon=inline-start]:pl-2 [&_svg:not([class*='size-'])]:size-3.5",
        lg: "h-10 gap-2 px-4 has-data-[icon=inline-end]:pr-3 has-data-[icon=inline-start]:pl-3",
        icon: "size-9",
        "icon-xs": "size-6 rounded-md [&_svg:not([class*='size-'])]:size-3",
        "icon-sm": "size-8 rounded-md",
        "icon-lg": "size-10",
      },
    },
    defaultVariants: {
      variant: "default",
      size: "default",
    },
  }
)

function Button({
  className,
  variant = "default",
  size = "default",
  ...props
}: ButtonPrimitive.Props & VariantProps<typeof buttonVariants>) {
  return (
    <ButtonPrimitive
      data-slot="button"
      className={cn(buttonVariants({ variant, size, className }))}
      {...props}
    />
  )
}

export { Button, buttonVariants }
