const BASE_URL = 'https://jsonplaceholder.typicode.com';

export async function get<T>(path: string): Promise<T> {
  const res = await fetch(BASE_URL + path);
  if (!res.ok) throw new Error('Request failed: ' + res.status);
  return res.json() as Promise<T>;
}

export async function post<T>(path: string, body: unknown): Promise<T> {
  const res = await fetch(BASE_URL + path, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  });
  if (!res.ok) throw new Error('Request failed: ' + res.status);
  return res.json() as Promise<T>;
}
