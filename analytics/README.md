# analytics

The [lakeshape](../lakeshape) project that turns the tables duckpipe loads
(`lake.uk.*`) into `staging` views and `marts` tables. Column meanings and
tests are in each model's `.yml`; this page is the map.

## Staging datasets

Each `stg_*` view types one raw table and applies its release rule (which
loads count). Coverage figures are from November 2026.

| Model | Source | One row per | Release rule | Key | Joins to ONSPD |
|---|---|---|---|---|---|
| `stg_onspd` | ONS Postcode Directory | UK postcode, live or terminated | latest release by the month in the file name | `pcds` | **core** |
| `stg_onspd_feb_2024` | ONSPD, February 2024 | UK postcode, as at Feb 2024 | that one file | `pcds` | `pcds` = `stg_onspd.pcds` (all but 25,700 newer postcodes) |
| `stg_lsoa_names` | ONS LSOA 2021 names | LSOA / data zone / super data zone | latest row per code | `lsoa21cd` | `lsoa21cd` = `stg_onspd.lsoa21cd` |
| `stg_msoa_names` | ONS MSOA 2021 names | MSOA / intermediate zone | latest row per code | `msoa21cd` | `msoa21cd` = `stg_onspd.msoa21cd` |
| `stg_ward_pcon19` | ONS ward → constituency → LAD, Dec 2019 | ward and constituency it falls in | latest file | `ward_pcon_key` | `pcon19cd` = `stg_onspd_feb_2024.pcon` |
| `stg_price_paid` | HM Land Registry price paid | sale (England and Wales, 1995 on), as it stands now | latest record per transaction, deletions dropped | `transaction_id` | `postcode` = `stg_onspd.pcds` (99.8%; 51k null) |
| `stg_crime_street` | data.police.uk street crime | crime or ASB incident per month (Sep 2020 on) | latest load of each file | none (`crime_id` null for ASB) | `lsoa_code` = `stg_onspd.lsoa21cd`, or `lsoa11cd` for older months (93% direct; 1.5M no location) |
| `stg_companies` | Companies House basic company data | live company | latest file | `companynumber` | `regaddresspostcode` = `stg_onspd.pcds` (98.1%; 91k null, the rest overseas or mistyped) |
| `stg_parliament_candidacies` | electionresults.parliament.uk | candidate in a constituency election (GE 2010–2024) | latest load of each file | `candidacy_key` | see [Elections](#elections) |

## Data model

ONSPD is the core: every other dataset reaches the geography through a
postcode, an LSOA or a constituency code.

```
                      dim_constituency  (1,295: pre-2024 and 2024 boundary sets)
                        ▲          ▲
   constituency_code    │          │  constituency_code
   constituency_pre2024_code       │
                        │      fct_election_result ◄── fct_election_candidacy
                        │                 (election_url)
  fct_price_paid ──► dim_geography ◄── dim_company
     (postcode)       (postcode)        (postcode)
                        │ lsoa_code
                        ▼
  fct_crime_street ──► dim_lsoa
     (lsoa_code)
```

| Fact / entity | Grain | postcode | LSOA | constituency | date |
|---|---|:-:|:-:|:-:|:-:|
| `fct_price_paid` | one sale | ✓ | via dim_geography | via dim_geography | `date_of_transfer` |
| `fct_crime_street` | one crime / ASB incident | | ✓ | via dim_lsoa | `month` |
| `dim_company` | one live company (latest release) | ✓ | via dim_geography | via dim_geography | `incorporation_date` |
| `fct_election_result` | one constituency election | | | ✓ | `election_date` |
| `fct_election_candidacy` | one candidate in one constituency election | | | ✓ | `election_date` |

The dimensions keep ONS codes as their keys (no surrogate keys), and facts
keep a null code where the source has none.

- `dim_geography`: one row per postcode, with its OA, LSOA, MSOA, ward,
  local authority, police force area, region, country, IMD rank and two
  constituencies (below).
- `dim_lsoa`: one row per 2021 LSOA, with the areas most of its postcodes
  are in, for data that only has an LSOA. LSOAs don't nest exactly in local
  authorities or constituencies: 1,786 of them straddle a 2024 constituency
  boundary.
- `dim_constituency`: one row per constituency code an election was fought
  in, with name, country, region and boundary dates.

## Elections

Each election's constituency is on the boundaries of its day, and ONSPD
only keeps the current ones, so a postcode gets one constituency per
boundary set:

| Elections | Boundary set | `constituency_geographic_code` matches | Coverage |
|---|---|---|---|
| 2024 | 2024 | `stg_onspd.pcon24cd` → `dim_geography.constituency_code` | 650 of 650 codes |
| 2010, 2015, 2017, 2019 | 2010 (Scotland 2005) | `stg_onspd_feb_2024.pcon`, by `pcds` → `dim_geography.constituency_pre2024_code` | 650 of 650 codes |

For the pre-2024 set, a postcode introduced after February 2024 isn't in
that release. It gets the constituency most of its 2021 output area's
postcodes were in (`is_constituency_pre2024_approximate`, 23,604
postcodes). Postcodes without a grid reference have neither.

To put a postcode or LSOA fact next to a result, join it to the election
on the code for that election's boundary set. For example, 2020 sales by
the party that won the seat in 2019:

```sql
select r.winning_party_abbreviation, count(*) as sales
from marts.fct_price_paid p
join marts.dim_geography g using (postcode)
join marts.fct_election_result r
  on r.constituency_code = g.constituency_pre2024_code
 and r.election_date = date '2019-12-12'
where p.date_of_transfer >= date '2020-01-01' and p.date_of_transfer < date '2021-01-01'
group by all
```

`stg_ward_pcon19` gives the same pre-2024 constituencies by ward. It is
coarser than the postcode route, so the model doesn't use it.
