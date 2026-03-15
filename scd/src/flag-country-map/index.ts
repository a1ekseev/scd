import type { CountryInfo } from '../types.ts';

const supportedRegions = [
  'AE', 'AR', 'AT', 'AU', 'BE', 'BG', 'BR', 'CA', 'CH', 'CO', 'CZ', 'DE', 'DK', 'ES',
  'FI', 'FR', 'GB', 'GR', 'HK', 'HR', 'HU', 'IE', 'IL', 'IN', 'IT', 'JP', 'KR', 'KZ',
  'LT', 'MX', 'MY', 'NL', 'NO', 'PE', 'PL', 'PT', 'RO', 'RU', 'SE', 'SG', 'SK', 'TR',
  'UA', 'US', 'ZA',
] as const;

const ruOverrides: Record<string, string> = {
  AE: 'ОАЭ',
  AR: 'Аргентина',
  AT: 'Австрия',
  AU: 'Австралия',
  BE: 'Бельгия',
  BG: 'Болгария',
  BR: 'Бразилия',
  CA: 'Канада',
  CH: 'Швейцария',
  CO: 'Колумбия',
  CZ: 'Чехия',
  DE: 'Германия',
  DK: 'Дания',
  ES: 'Испания',
  FI: 'Финляндия',
  FR: 'Франция',
  GB: 'Великобритания',
  GR: 'Греция',
  HK: 'Гонконг',
  HR: 'Хорватия',
  HU: 'Венгрия',
  IE: 'Ирландия',
  IL: 'Израиль',
  IN: 'Индия',
  IT: 'Италия',
  JP: 'Япония',
  KR: 'Южная Корея',
  KZ: 'Казахстан',
  LT: 'Литва',
  MX: 'Мексика',
  MY: 'Малайзия',
  NL: 'Нидерланды',
  NO: 'Норвегия',
  PE: 'Перу',
  PL: 'Польша',
  PT: 'Португалия',
  RO: 'Румыния',
  RU: 'Россия',
  SE: 'Швеция',
  SG: 'Сингапур',
  SK: 'Словакия',
  TR: 'Турция',
  UA: 'Украина',
  US: 'США',
  ZA: 'ЮАР',
};

const enDisplay = new Intl.DisplayNames(['en'], { type: 'region' });

function isoToEmoji(iso2: string): string {
  return [...iso2.toUpperCase()]
    .map((char) => String.fromCodePoint(0x1f1e6 + char.charCodeAt(0) - 65))
    .join('');
}

export const FLAG_COUNTRY_MAP: Record<string, CountryInfo> = Object.fromEntries(
  supportedRegions.map((iso2) => {
    const emoji = isoToEmoji(iso2);
    return [
      emoji,
      {
        emoji,
        iso2,
        nameEn: enDisplay.of(iso2) ?? iso2,
        nameRu: ruOverrides[iso2] ?? iso2,
      },
    ];
  }),
);

export function extractCountryInfo(label: string): CountryInfo | undefined {
  const emoji = [...label].slice(0, 2).join('');
  return FLAG_COUNTRY_MAP[emoji];
}
