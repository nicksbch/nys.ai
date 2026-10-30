-- ONS MSOA (2021) names and codes, one row per area: England and Wales
-- MSOAs and Scotland's intermediate zones (2022), from one file each.
-- Northern Ireland has none. A newer file for a country gives its areas new
-- names, so the latest row of each code is kept.
with raw as (
    -- duckpipe keeps each CSV row in record, every value as text. The pipe
    -- names the columns, as each country's file names its code differently.
    select
        record->>'msoa21cd' as msoa21cd,
        record->>'msoa21nm' as msoa21nm,
        _file,
        _loaded_at
    from {{ source('uk', 'msoa_names') }}
    qualify row_number() over (partition by msoa21cd order by _loaded_at desc, _file desc) = 1
)
select
    msoa21cd,
    msoa21nm,
    _file,
    _loaded_at
from raw
