-- One row per UK postcode, live or terminated, with where it is and the
-- areas it falls in (latest ONSPD release). The lowest level datasets with a
-- postcode join to; datasets with only an LSOA join to dim_lsoa, whose
-- lsoa_code is this table's.
select
    pcds as postcode,
    doterm is null as is_live,
    dointr as introduced_month,
    doterm as terminated_month,
    lat as latitude,
    long as longitude,
    oa21cd as oa_code,
    lsoa21cd as lsoa_code,
    msoa21cd as msoa_code,
    wd25cd as ward_code,
    lad25cd as local_authority_code,
    pcon24cd as constituency_code,
    pfa23cd as police_force_area_code,
    rgn25cd as region_code,
    ctry25cd as country_code,
    imd20ind as imd_rank
from {{ ref('stg_onspd') }}
