export function ingressEndpointURL(endpoint: string, source: 'none' | 'secret' | 'certManager') {
  const normalized = endpoint.replace(/^https?:\/\//i, `${source === 'none' ? 'http' : 'https'}://`);
  if (normalized !== normalized.trim()) return normalized;
  try {
    const parsed = new URL(normalized);
    if (
      (parsed.protocol === 'http:' || parsed.protocol === 'https:') &&
      !parsed.username &&
      !parsed.password &&
      (parsed.pathname === '' || parsed.pathname === '/') &&
      !parsed.search &&
      !parsed.hash
    )
      return parsed.origin;
  } catch {
    return normalized;
  }
  return normalized;
}

export function loadBalancerSSHAddress(address: string) {
  if (address !== address.trim() || !address) return address;
  try {
    const parsed = new URL(`ssh://${address}`);
    if (!parsed.hostname || parsed.username || parsed.password || parsed.pathname || parsed.search || parsed.hash)
      return address;
    const host = parsed.hostname.includes(':') ? `[${parsed.hostname}]` : parsed.hostname;
    return `${host}:22`;
  } catch {
    return address;
  }
}
