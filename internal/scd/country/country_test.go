package country

import "testing"

func TestFromLabelAustria(t *testing.T) {
	got, ok := FromLabel("🇦🇹 Вена, Австрия, Extra")
	want := Info{Emoji: "🇦🇹", ISO2: "AT", NameEn: "Austria", NameRu: "Австрия"}
	if !ok || got != want {
		t.Fatalf("got %+v %v, want %+v", got, ok, want)
	}
}

func TestFromLabelNewCodes(t *testing.T) {
	for label, want := range map[string]Info{
		"🇳🇬 Nigeria, Lagos": {Emoji: "🇳🇬", ISO2: "NG", NameEn: "Nigeria", NameRu: "Нигерия"},
		"🇮🇸 Reykjavik":      {Emoji: "🇮🇸", ISO2: "IS", NameEn: "Iceland", NameRu: "Исландия"},
	} {
		if got, ok := FromLabel(label); !ok || got != want {
			t.Errorf("%s: got %+v %v, want %+v", label, got, ok, want)
		}
	}
}

func TestFromLabelOutsideMap(t *testing.T) {
	for _, l := range []string{"🇽🇰 Pristina", "🇺🇳 New York", "🇪🇺 Brussels", "🇿🇿"} {
		if _, ok := FromLabel(l); ok {
			t.Errorf("%q must not resolve", l)
		}
	}
}

func TestFromLabelLeadingSpace(t *testing.T) {
	for _, l := range []string{" 🇩🇪 Berlin", "\uFEFF🇩🇪", "", "🇩", "DE"} {
		if _, ok := FromLabel(l); ok {
			t.Errorf("%q must not resolve", l)
		}
	}
}

func TestEveryCodeHasNamesAndEmoji(t *testing.T) {
	if len(table) != len(codes) {
		t.Fatalf("table %d, codes %d", len(table), len(codes))
	}
	for _, c := range codes {
		emoji := string([]rune{regionalA + rune(c[0]-'A'), regionalA + rune(c[1]-'A')})
		info, ok := FromLabel(emoji)
		if !ok || info.ISO2 != c || info.Emoji != emoji || info.NameEn == "" || info.NameRu == "" {
			t.Errorf("%s: %+v %v", c, info, ok)
		}
	}
}

func TestICUNames(t *testing.T) {
	for iso, en := range map[string]string{"TR": "Türkiye", "HK": "Hong Kong SAR China", "CZ": "Czechia", "KR": "South Korea"} {
		if table[iso].en != en {
			t.Errorf("%s: %q", iso, table[iso].en)
		}
	}
}
