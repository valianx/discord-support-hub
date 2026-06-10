// Runtime settings — hub base URL only.
// The API key is no longer handled in the browser: nginx injects the
// Authorization header server-side from the BACKOFFICE_API_KEY env var.

// The base URL is always same-origin ("/") in the nginx-served deployment.
// Keeping this helper avoids scattering the constant through the codebase.
export function getBaseUrl(): string {
  return '/'
}
