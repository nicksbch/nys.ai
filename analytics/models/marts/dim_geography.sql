-- One row per UK postcode, live or terminated, with where it is and the
-- areas it falls in (latest ONSPD release). The lowest level datasets with a
-- postcode join to; datasets with only an LSOA join to dim_lsoa, whose
-- lsoa_code is this table's.
--
-- Two constituencies per postcode, one per boundary set, so every general
-- election joins: constituency_code (2024 boundaries, the 2024 election) and
-- constituency_pre2024_code (2010 boundaries, Scotland 2005: the 2010-2019
-- elections). The latest release only has the 2024 ones, so the older ones
-- come from the February 2024 release, by postcode. Postcodes introduced
-- since then get the constituency most of their 2021 output area's postcodes
-- were in.
with feb_2024 as (
    select pcds, pcon
    from {{ ref('stg_onspd_feb_2024') }}
),

oa_pcon as (
    select oa21, mode(pcon) as pcon
    from {{ ref('stg_onspd_feb_2024') }}
    where oa21 is not null and pcon is not null
    group by oa21
)
select
    o.pcds as postcode,
    o.doterm is null as is_live,
    o.dointr as introduced_month,
    o.doterm as terminated_month,
    o.lat as latitude,
    o.long as longitude,
    o.oa21cd as oa_code,
    o.lsoa21cd as lsoa_code,
    o.msoa21cd as msoa_code,
    o.wd25cd as ward_code,
    o.lad25cd as local_authority_code,
    o.pcon24cd as constituency_code,
    coalesce(f.pcon, op.pcon) as constituency_pre2024_code,
    f.pcon is null and op.pcon is not null as is_constituency_pre2024_approximate,
    o.pfa23cd as police_force_area_code,
    o.rgn25cd as region_code,
    o.ctry25cd as country_code,
    o.imd20ind as imd_rank
from {{ ref('stg_onspd') }} o
left join feb_2024 f on f.pcds = o.pcds
left join oa_pcon op on op.oa21 = o.oa21cd
