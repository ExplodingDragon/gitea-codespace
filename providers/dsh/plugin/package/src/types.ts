/** Visibility enforced by the Codespace Gateway for a published port. */
export type EndpointVisibility = 'authenticated' | 'public'

/** One port published from the current Codespace. */
export interface Endpoint {
  /** TCP port inside the Dev Container. */
  port: number
  /** User-facing service name. */
  label: string
  /** Whether Gateway access requires the Codespace user session. */
  visibility: EndpointVisibility
}
