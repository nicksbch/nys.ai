-- One row per 2021 LSOA (data zone in Scotland, super data zone in Northern
-- Ireland), with the larger areas it falls in, from the postcodes in it
-- (latest ONSPD release). For datasets whose lowest level is the LSOA.
-- MSOAs nest in LSOAs exactly; a few LSOAs straddle 2025 local authority,
-- police force area or constituency boundaries, and get the one most of their
-- postcodes are in. The pre-2024 constituency is the one most of its
-- postcodes were in in the February 2024 release.
select
    o.lsoa21cd as lsoa_code,
    any_value(o.msoa21cd) as msoa_code,
    mode(o.lad25cd) as local_authority_code,
    mode(o.pcon24cd) as constituency_code,
    mode(f.pcon) as constituency_pre2024_code,
    mode(o.pfa23cd) as police_force_area_code,
    mode(o.rgn25cd) as region_code,
    any_value(o.ctry25cd) as country_code,
    count(*) as postcodes,
    count(*) filter (where o.doterm is null) as live_postcodes
from {{ ref('stg_onspd') }} o
left join {{ ref('stg_onspd_feb_2024') }} f on f.pcds = o.pcds
where o.lsoa21cd is not null
group by o.lsoa21cd
