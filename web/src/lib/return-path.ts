// safeReturn keeps the post-login redirect inside this console. The login
// page's ?return= comes from a URL, so an absolute or protocol-relative value
// (which browsers also spell with a leading backslash) must not win.
export function safeReturn(value: string | undefined): string {
  if (!value || !value.startsWith('/') || value.startsWith('//') || value.startsWith('/\\')) {
    return '/'
  }
  return value
}
