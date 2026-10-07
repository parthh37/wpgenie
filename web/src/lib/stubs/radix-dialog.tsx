// cmdk imports @radix-ui/react-dialog for its Command.Dialog, which the
// panel doesn't use (the palette is a Base UI dialog around cmdk's list).
// Radix's dialog brings react-remove-scroll, which injects <style> elements
// the CSP blocks; vite.config.ts points the import here instead.
const unused = () => {
  throw new Error("cmdk's Command.Dialog is not available: use the Base UI dialog")
}
export const Root = unused
export const Portal = unused
export const Overlay = unused
export const Content = unused
