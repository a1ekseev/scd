import type { ManifestEntry, OutboundManifest, SubscriptionFiltersConfig } from '../types.ts';

function parseRegexLiteral(value: string): RegExp {
  const lastSlash = value.lastIndexOf('/');
  const pattern = value.slice(1, lastSlash);
  const flags = value.slice(lastSlash + 1);
  return new RegExp(pattern, flags);
}

function applyCountryFilter(entries: ManifestEntry[], countryAllowlist: Set<string>): {
  entries: ManifestEntry[];
  filteredByCountry: number;
} {
  const kept: ManifestEntry[] = [];
  let filteredByCountry = 0;

  for (const entry of entries) {
    const iso2 = entry.country?.iso2;
    if (!iso2 || !countryAllowlist.has(iso2)) {
      filteredByCountry += 1;
      continue;
    }
    kept.push(entry);
  }

  return {
    entries: kept,
    filteredByCountry,
  };
}

function applyLabelRegexFilter(entries: ManifestEntry[], regex: RegExp): {
  entries: ManifestEntry[];
  filteredByLabelRegex: number;
} {
  const kept: ManifestEntry[] = [];
  let filteredByLabelRegex = 0;

  for (const entry of entries) {
    regex.lastIndex = 0;
    if (!regex.test(entry.label)) {
      filteredByLabelRegex += 1;
      continue;
    }
    kept.push(entry);
  }

  return {
    entries: kept,
    filteredByLabelRegex,
  };
}

export function applyManifestFilters(
  manifest: OutboundManifest,
  filters?: SubscriptionFiltersConfig,
): OutboundManifest {
  if (!filters?.countryAllowlist?.length && !filters?.labelIncludeRegex) {
    return manifest;
  }

  let entries = manifest.entries;
  let filteredByCountry = 0;
  let filteredByLabelRegex = 0;

  if (filters.countryAllowlist?.length) {
    const result = applyCountryFilter(entries, new Set(filters.countryAllowlist));
    entries = result.entries;
    filteredByCountry = result.filteredByCountry;
  }

  if (filters.labelIncludeRegex) {
    const result = applyLabelRegexFilter(entries, parseRegexLiteral(filters.labelIncludeRegex));
    entries = result.entries;
    filteredByLabelRegex = result.filteredByLabelRegex;
  }

  return {
    ...manifest,
    entries,
    summary: {
      ...manifest.summary,
      parsed: entries.length,
      filtered: filteredByCountry + filteredByLabelRegex,
      filteredByCountry,
      filteredByLabelRegex,
    },
  };
}
