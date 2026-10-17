package country

import "strings"

// Info describes a country resolved from a flag emoji (01 §7).
type Info struct {
	Emoji  string `json:"emoji"`
	ISO2   string `json:"iso2"`
	NameEn string `json:"nameEn"`
	NameRu string `json:"nameRu"`
}

type names struct{ en, ru string }

// table holds the English names as Node (ICU) renders them, hard-coded, and
// the Russian names of the original flag map.
var table = map[string]names{
	"AE": {"United Arab Emirates", "ОАЭ"},
	"AR": {"Argentina", "Аргентина"},
	"AT": {"Austria", "Австрия"},
	"AU": {"Australia", "Австралия"},
	"BE": {"Belgium", "Бельгия"},
	"BG": {"Bulgaria", "Болгария"},
	"BR": {"Brazil", "Бразилия"},
	"CA": {"Canada", "Канада"},
	"CH": {"Switzerland", "Швейцария"},
	"CO": {"Colombia", "Колумбия"},
	"CZ": {"Czechia", "Чехия"},
	"DE": {"Germany", "Германия"},
	"DK": {"Denmark", "Дания"},
	"ES": {"Spain", "Испания"},
	"FI": {"Finland", "Финляндия"},
	"FR": {"France", "Франция"},
	"GB": {"United Kingdom", "Великобритания"},
	"GR": {"Greece", "Греция"},
	"HK": {"Hong Kong SAR China", "Гонконг"},
	"HR": {"Croatia", "Хорватия"},
	"HU": {"Hungary", "Венгрия"},
	"IE": {"Ireland", "Ирландия"},
	"IL": {"Israel", "Израиль"},
	"IN": {"India", "Индия"},
	"IT": {"Italy", "Италия"},
	"JP": {"Japan", "Япония"},
	"KR": {"South Korea", "Южная Корея"},
	"KZ": {"Kazakhstan", "Казахстан"},
	"LT": {"Lithuania", "Литва"},
	"MX": {"Mexico", "Мексика"},
	"MY": {"Malaysia", "Малайзия"},
	"NL": {"Netherlands", "Нидерланды"},
	"NO": {"Norway", "Норвегия"},
	"PE": {"Peru", "Перу"},
	"PL": {"Poland", "Польша"},
	"PT": {"Portugal", "Португалия"},
	"RO": {"Romania", "Румыния"},
	"RU": {"Russia", "Россия"},
	"SE": {"Sweden", "Швеция"},
	"SG": {"Singapore", "Сингапур"},
	"SK": {"Slovakia", "Словакия"},
	"TR": {"Türkiye", "Турция"},
	"UA": {"Ukraine", "Украина"},
	"US": {"United States", "США"},
	"ZA": {"South Africa", "ЮАР"},
}

const regionalA = 0x1F1E6

// FromLabel resolves the country from the first two runes of label, which must
// be the flag of one of the supported countries. Anything before the flag
// (space, BOM) means no country.
func FromLabel(label string) (Info, bool) {
	var iso strings.Builder
	var flag []rune
	n := 0
	for _, r := range label {
		if r < regionalA || r > regionalA+25 {
			return Info{}, false
		}
		iso.WriteByte(byte('A' + r - regionalA))
		flag = append(flag, r)
		n++
		if n == 2 {
			break
		}
	}
	if n < 2 {
		return Info{}, false
	}
	nm, ok := table[iso.String()]
	if !ok {
		return Info{}, false
	}
	return Info{Emoji: string(flag), ISO2: iso.String(), NameEn: nm.en, NameRu: nm.ru}, true
}
