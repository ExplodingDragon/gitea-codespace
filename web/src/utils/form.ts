// Administration forms contain JSON API data. The JSON round trip matches the
// request payload and removes Vue proxies before callers validate or mutate it.
export function cloneFormValue<T extends object>(value: T): T {
  return JSON.parse(JSON.stringify(value)) as T;
}
