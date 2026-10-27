-- One row per data.police.uk street-level crime or anti-social behaviour
-- incident, keyed by its 2021 LSOA to dim_lsoa. There is no postcode: the
-- published point is moved to an anonymous nearby spot, so the LSOA is the
-- lowest level. Until mid-2023 police.uk used 2011 LSOAs; codes that did not
-- change in 2021 are kept, the rest are mapped to the 2021 LSOA holding most
-- of the 2011 LSOA's postcodes.
with lsoa21 as (
    select distinct lsoa21cd
    from {{ ref('stg_onspd') }}
    where lsoa21cd is not null
),

lsoa11 as (
    select lsoa11cd, lsoa21cd, count(*) as postcodes
    from {{ ref('stg_onspd') }}
    where lsoa11cd is not null and lsoa21cd is not null
    group by all
),

lsoa11_to_21 as (
    select
        lsoa11cd,
        arg_max(lsoa21cd, postcodes) as lsoa21cd,
        count(*) > 1 as is_split  -- the 2011 LSOA is now in more than one
    from lsoa11
    group by lsoa11cd
)
select
    c.crime_id,
    c.month,
    c.crime_type,
    c.last_outcome_category,
    c.reported_by,
    c.falls_within,
    coalesce(l21.lsoa21cd, m.lsoa21cd) as lsoa_code,
    nullif(c.lsoa_code, '') as reported_lsoa_code,
    l21.lsoa21cd is null and coalesce(m.is_split, false) as is_lsoa_approximate,
    c.latitude,
    c.longitude
from {{ ref('stg_crime_street') }} c
left join lsoa21 l21 on l21.lsoa21cd = c.lsoa_code
left join lsoa11_to_21 m on m.lsoa11cd = c.lsoa_code
