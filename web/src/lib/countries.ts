// Countries (ISO 3166-1 alpha-2 codes), named in the browser's language:
// the legacy countries.js, for addresses, tax rules and Protection's
// country rule.

export const COUNTRY_CODES = (
  "AD AE AF AG AI AL AM AO AQ AR AS AT AU AW AX AZ BA BB BD BE BF BG BH BI BJ BL BM BN BO BQ BR BS BT BV " +
  "BW BY BZ CA CC CD CF CG CH CI CK CL CM CN CO CR CU CV CW CX CY CZ DE DJ DK DM DO DZ EC EE EG EH ER ES ET FI FJ FK FM " +
  "FO FR GA GB GD GE GF GG GH GI GL GM GN GP GQ GR GS GT GU GW GY HK HM HN HR HT HU ID IE IL IM IN IO IQ IR IS IT JE JM " +
  "JO JP KE KG KH KI KM KN KP KR KW KY KZ LA LB LC LI LK LR LS LT LU LV LY MA MC MD ME MF MG MH MK ML MM MN MO MP MQ MR " +
  "MS MT MU MV MW MX MY MZ NA NC NE NF NG NI NL NO NP NR NU NZ OM PA PE PF PG PH PK PL PM PN PR PS PT PW PY QA RE RO RS " +
  "RU RW SA SB SC SD SE SG SH SI SJ SK SL SM SN SO SR SS ST SV SX SY SZ TC TD TF TG TH TJ TK TL TM TN TO TR TT TV TW TZ " +
  "UA UG UM US UY UZ VA VC VE VG VI VN VU WF WS YE YT ZA ZM ZW"
).split(" ")

export const COUNTRY_SET: ReadonlySet<string> = new Set(COUNTRY_CODES)

// Countries whose addresses need a state or province: their taxes depend on it.
export const STATE_COUNTRIES = ["US", "CA", "IN", "AU"]

const REGION_NAMES = (() => {
  try {
    return new Intl.DisplayNames(undefined, { type: "region" })
  } catch {
    return null
  }
})()

export function countryName(code: string | null | undefined) {
  if (!code) return ""
  try {
    return REGION_NAMES?.of(code) || code
  } catch {
    return code
  }
}

let LIST: Array<[code: string, name: string]> | null = null

// countryList is [[code, name], …] sorted by name.
export function countryList() {
  if (!LIST) LIST = COUNTRY_CODES.map((c): [string, string] => [c, countryName(c)]).sort((a, b) => a[1].localeCompare(b[1]))
  return LIST
}

// guessCountry is the browser's region (en-GB: GB), "" when it has none.
export function guessCountry() {
  const m = /-([A-Za-z]{2})\b/.exec(navigator.language || "")
  const c = m ? m[1].toUpperCase() : ""
  return COUNTRY_CODES.includes(c) ? c : ""
}
