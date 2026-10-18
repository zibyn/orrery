// safeReturn keeps the post-login redirect inside this console. The login
// page's ?return= comes from a URL, so an absolute or protocol-relative value
// (which browsers also spell with a leading backslash) must not win. Browsers
// strip tabs and newlines from URLs, so control characters are rejected too:
// "/\t/evil" would otherwise become the protocol-relative "//evil".
export function safeReturn(value: string | undefined): string {
  if (!value || !value.startsWith('/') || value.startsWith('//') || value.startsWith('/\\')) {
    return '/'
  }
  if (/[\u0000-\u001f\u007f]/.test(value)) {    return '/'
  }
  return value
}
