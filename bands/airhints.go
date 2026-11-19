package bands

import (
	"strings"
	"unicode"
)

// Air-label rule: a port of AirLabelHints in pool-apps
// (shared/core/src/commonMain/kotlin/cloud/labonte/pool/core/scale/AirLabelHints.kt, Swift mirror
// apps/ios/UICommon/Sources/UICommon/Types/AirLabelHints.swift). Keep the four lists and the rule
// identical to the Kotlin source; the shared cases live in testdata/measurement-parity.json.
//
// Neither controller reports what a 1-Wire probe measures, so the name the owner typed into the
// controller is the only signal, in the owner's language. The terms of every enforced app language
// apply at once. Matching is per word, never per substring ("chair", "Luftsprudler" stay water): a
// word is air when it is in airWords, or a compound of an airStems entry and one or more airTails
// entries (each optionally after a German linking "s"). Decimal digits at the end of a word are
// ignored ("Außen1"). A name that also carries a waterWords entry ("Outdoor pool") stays water.
//
// A word is a maximal run of runes that are Unicode letters (L*) or decimal digits (Nd), which is
// what Kotlin's Char.isLetterOrDigit accepts for every Basic Multilingual Plane character and what
// the Swift mirror accepts per scalar.

// airWords are whole words that mean outdoor / ambient air, per enforced language.
var airWords = setOf(
	// en
	"air", "ambient", "outdoor", "outdoors", "outside", "exterior", "ext",
	// de
	"außen", "aussen", "draußen", "draussen", "luft", "umgebung",
	// es ("exterior" is listed under en)
	"aire", "ambiente", "ambiental", "fuera",
	// fr
	"extérieur", "extérieure", "exterieur", "exterieure", "ambiant", "ambiante", "dehors",
	// it ("ambiente" is listed under es)
	"aria", "esterno", "esterna", "fuori",
	// nb
	"ute", "utendørs", "utendors", "omgivelse", "omgivelser", "omgivelsene",
	// pl
	"powietrze", "powietrza", "zewnętrzna", "zewnętrzny", "zewnętrzne", "zewnątrz", "otoczenie", "otoczenia",
	// uk
	"повітря", "зовнішня", "зовнішній", "зовнішнє", "надворі", "навколишнього", "довкілля", "вулиця", "вулиці",
)

// airStems are the first half of a compound air word (German / Norwegian compounding, plus "airtemp").
var airStems = []string{
	"außen", "aussen", "luft", "umgebung", "ute", "utendørs", "utendors", "omgivelse", "air", "outdoor",
}

// airTails are the second half of a compound air word: the measurement or the sensor itself.
var airTails = []string{"temperatur", "temperature", "temp", "fühler", "føler", "sensor", "luft"}

// waterWords put a name on the water side, whatever air word sits next to them.
var waterWords = setOf(
	// en
	"pool", "basin", "water", "spa", "whirlpool",
	// de
	"becken", "schwimmbad", "wasser", "beckenwasser",
	// es / it
	"piscina", "agua", "acqua",
	// fr
	"piscine", "bassin", "eau",
	// nb
	"basseng", "svømmebasseng", "vann",
	// pl
	"basen", "basenu", "woda", "wody",
	// uk
	"басейн", "басейну", "вода", "води",
)

func setOf(words ...string) map[string]struct{} {
	s := make(map[string]struct{}, len(words))
	for _, w := range words {
		s[w] = struct{}{}
	}
	return s
}

// labelHintsAir reports whether hint (trimmed + lowercased) names an air sensor.
func labelHintsAir(hint string) bool {
	air := false
	for _, w := range strings.FieldsFunc(hint, isNotLetterOrDigit) {
		w = withoutTrailingDigits(w)
		if _, water := waterWords[w]; water {
			return false
		}
		if !air && isAirWord(w) {
			air = true
		}
	}
	return air
}

func isNotLetterOrDigit(r rune) bool {
	return !unicode.IsLetter(r) && !unicode.IsDigit(r)
}

// withoutTrailingDigits turns "außen1" into "außen": a sensor number glued to the word does not
// change what it names. An all-digit word stays as it is.
func withoutTrailingDigits(word string) string {
	if trimmed := strings.TrimRightFunc(word, unicode.IsDigit); trimmed != "" {
		return trimmed
	}
	return word
}

func isAirWord(word string) bool {
	if _, ok := airWords[word]; ok {
		return true
	}
	for _, stem := range airStems {
		if rest, ok := strings.CutPrefix(word, stem); ok && isTailChain(rest) {
			return true
		}
	}
	return false
}

// isTailChain reports whether rest is one or more airTails in a row, each optionally after a
// linking "s": "temperatur", "stemperatur", "temperaturfühler", "slufttemperatur". Every split is
// tried, since one tail can be the start of another ("temp" / "temperatur").
func isTailChain(rest string) bool {
	if isTailChainWithoutLink(rest) {
		return true
	}
	unlinked, ok := strings.CutPrefix(rest, "s")
	return ok && isTailChainWithoutLink(unlinked)
}

func isTailChainWithoutLink(rest string) bool {
	for _, tail := range airTails {
		if after, ok := strings.CutPrefix(rest, tail); ok && (after == "" || isTailChain(after)) {
			return true
		}
	}
	return false
}
