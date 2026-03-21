export function buildOutputPath(pathRoute: string, id: string): string {
  return `${pathRoute}/${encodeURIComponent(id)}`;
}
