// CONTRACT (owner: billing-core agent). A billing contact's fields
// (uncontrolled, for a <form>; names as the API's), shared by staff and the
// client area. readContact turns the form back into the API's shape.
// eslint-disable-next-line @typescript-eslint/no-explicit-any
export type Contact = Record<string, any>

export function ContactFields(_props: { contact?: Contact }) {
  return null
}
export function readContact(_form: HTMLFormElement): Contact {
  return {}
}
