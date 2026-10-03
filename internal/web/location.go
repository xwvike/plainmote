package web

import (
	"net/http"
	"strings"
	"unicode"

	"golang.org/x/text/language"
	"golang.org/x/text/language/display"

	"plainmote/internal/store"
)

// locationPartMaxBytes bounds a region or city name. Real ones are far
// shorter; anything longer is not a place name.
const locationPartMaxBytes = 96

// visitorLocation is where the proxy in front estimated the caller to be. Like
// the client IP, the headers are believed only from a trusted proxy: anyone
// else can send a cf-ipcity of their choosing. Cloudflare reports an unknown
// country as XX, and a location is only as good as its country, so then
// nothing is kept.
func (a *App) visitorLocation(r *http.Request) store.Location {
	if !a.cfg.CloudflareLocation || !a.behindTrustedProxy(r) {
		return store.Location{}
	}
	country := strings.ToUpper(strings.TrimSpace(r.Header.Get("CF-IPCountry")))
	if !countryCode(country) || country == "XX" {
		return store.Location{}
	}
	return store.Location{
		Country: country,
		Region:  locationPart(r.Header.Get("CF-Region")),
		City:    locationPart(r.Header.Get("CF-IPCity")),
	}
}

// countryCode accepts an ISO 3166-1 alpha-2 code, and T1, which is what
// Cloudflare reports for Tor.
func countryCode(code string) bool {
	if len(code) != 2 {
		return false
	}
	for _, c := range []byte(code) {
		if (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			return false
		}
	}
	return code[0] >= 'A' && code[0] <= 'Z'
}

// locationPart cleans a place name for display. Cloudflare sends names
// outside ASCII as raw UTF-8; whatever else arrives is dropped rather than
// stored, since it is shown back to the owner as text.
func locationPart(value string) string {
	value = strings.ToValidUTF8(value, "")
	value = strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return ' '
		}
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, value)
	value = strings.Join(strings.Fields(value), " ")
	if len(value) > locationPartMaxBytes {
		return ""
	}
	return value
}

// countryName is the country in the reader's language. Region and city stay
// as the proxy named them: there is no table to translate them with.
func countryName(locale, code string) string {
	switch code {
	case "":
		return ""
	case "T1":
		return "Tor"
	}
	region, err := language.ParseRegion(code)
	if err != nil {
		return code
	}
	dictionary, ok := countryDictionaries[locale]
	if !ok {
		dictionary = display.English
	}
	if name := dictionary.Regions().Name(region); name != "" {
		return name
	}
	return code
}

// countryDictionaries maps each interface language to its CLDR names.
var countryDictionaries = map[string]*display.Dictionary{
	"en":    display.English,
	"zh-CN": display.SimplifiedChinese,
	"zh-TW": display.TraditionalChinese,
	"ja":    display.Japanese,
	"fr":    display.French,
	"de":    display.German,
}

// locationShort is the location for a list row, where the region would
// mostly push the line past its column: the country and the most precise
// place known.
func locationShort(locale string, location store.Location) string {
	place := location.City
	if place == "" {
		place = location.Region
	}
	return locationText(locale, store.Location{Country: location.Country, City: place})
}

// locationText is the location as one line, widest first. A city that is
// its own region (Beijing, Singapore) is not named twice.
func locationText(locale string, location store.Location) string {
	parts := make([]string, 0, 3)
	if name := countryName(locale, location.Country); name != "" {
		parts = append(parts, name)
	}
	if location.Region != "" {
		parts = append(parts, location.Region)
	}
	if location.City != "" && !strings.EqualFold(location.City, location.Region) {
		parts = append(parts, location.City)
	}
	return strings.Join(parts, " · ")
}
