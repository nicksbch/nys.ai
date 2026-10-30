-- ONS LSOA (2021) names and codes, one row per small area: England and
-- Wales LSOAs, Scotland's data zones (2022) and Northern Ireland's super data
-- zones, from one file each. A newer file for a country gives its areas new
-- names, so the latest row of each code is kept.
with raw as (
    -- duckpipe keeps each CSV row in record, every value as text. The pipe
    -- names the columns, as each country's file names its code differently.
    select
        record->>'lsoa21cd' as lsoa21cd,
        record->>'lsoa21nm' as lsoa21nm,
        _file,
        _loaded_at
    from {{ source('uk', 'lsoa_names') }}
    qualify row_number() over (partition by lsoa21cd order by _loaded_at desc, _file desc) = 1
)
select
    lsoa21cd,
    lsoa21nm,
    _file,
    _loaded_at
from raw
