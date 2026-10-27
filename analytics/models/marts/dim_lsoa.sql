-- One row per 2021 LSOA (data zone in Scotland, super data zone in Northern
-- Ireland), with the larger areas it falls in, from the postcodes in it
-- (latest ONSPD release). For datasets whose lowest level is the LSOA.
-- MSOAs nest in LSOAs exactly; a few LSOAs straddle 2025 local authority or
-- police force area boundaries, and get the one most of their postcodes are in.
select
    lsoa21cd as lsoa_code,
    any_value(msoa21cd) as msoa_code,
    mode(lad25cd) as local_authority_code,
    mode(pfa23cd) as police_force_area_code,
    mode(rgn25cd) as region_code,
    any_value(ctry25cd) as country_code,
    count(*) as postcodes,
    count(*) filter (where doterm is null) as live_postcodes
from {{ ref('stg_onspd') }}
where lsoa21cd is not null
group by lsoa21cd
