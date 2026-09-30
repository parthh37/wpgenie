'use strict';
// Countries for addresses and tax rules (ISO 3166-1 alpha-2 codes), named
// in the browser's language. Shared by the dashboard and the order page.

const COUNTRY_CODES = ('AD AE AF AG AI AL AM AO AQ AR AS AT AU AW AX AZ BA BB BD BE BF BG BH BI BJ BL BM BN BO BQ BR BS BT BV ' +
  'BW BY BZ CA CC CD CF CG CH CI CK CL CM CN CO CR CU CV CW CX CY CZ DE DJ DK DM DO DZ EC EE EG EH ER ES ET FI FJ FK FM ' +
  'FO FR GA GB GD GE GF GG GH GI GL GM GN GP GQ GR GS GT GU GW GY HK HM HN HR HT HU ID IE IL IM IN IO IQ IR IS IT JE JM ' +
  'JO JP KE KG KH KI KM KN KP KR KW KY KZ LA LB LC LI LK LR LS LT LU LV LY MA MC MD ME MF MG MH MK ML MM MN MO MP MQ MR ' +
  'MS MT MU MV MW MX MY MZ NA NC NE NF NG NI NL NO NP NR NU NZ OM PA PE PF PG PH PK PL PM PN PR PS PT PW PY QA RE RO RS ' +
  'RU RW SA SB SC SD SE SG SH SI SJ SK SL SM SN SO SR SS ST SV SX SY SZ TC TD TF TG TH TJ TK TL TM TN TO TR TT TV TW TZ ' +
  'UA UG UM US UY UZ VA VC VE VG VI VN VU WF WS YE YT ZA ZM ZW').split(' ');

// Countries whose addresses need a state or province: their taxes depend on it.
const STATE_COUNTRIES = ['US', 'CA', 'IN', 'AU'];

const REGION_NAMES = (() => {
  try { return new Intl.DisplayNames(undefined, { type: 'region' }); } catch (e) { return null; }
})();

function countryName(code) {
  if (!code) return '';
  try { return (REGION_NAMES && REGION_NAMES.of(code)) || code; } catch (e) { return code; }
}

let COUNTRY_LIST = null;

// countryList is [[code, name], …] sorted by name.
function countryList() {
  if (!COUNTRY_LIST) {
    COUNTRY_LIST = COUNTRY_CODES.map((c) => [c, countryName(c)]).sort((a, b) => a[1].localeCompare(b[1]));
  }
  return COUNTRY_LIST;
}

// guessCountry is the browser's region (en-GB: GB), '' when it has none.
function guessCountry() {
  const m = /-([A-Za-z]{2})\b/.exec(navigator.language || '');
  const c = m ? m[1].toUpperCase() : '';
  return COUNTRY_CODES.includes(c) ? c : '';
}
