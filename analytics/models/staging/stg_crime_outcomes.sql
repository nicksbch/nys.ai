-- data.police.uk outcomes, one row per outcome recorded for a crime in a month.
-- A crime can have several. Only the latest load of each file is kept, as in
-- stg_crime_street.
with raw as (
    -- duckpipe keeps each CSV row in record, every value as text.
    select
        record->>'crime_id' as crime_id,
        record->>'_month' as month,
        record->>'reported_by' as reported_by,
        record->>'falls_within' as falls_within,
        record->>'longitude' as longitude,
        record->>'latitude' as latitude,
        record->>'_location' as location,
        record->>'lsoa_code' as lsoa_code,
        record->>'lsoa_name' as lsoa_name,
        record->>'outcome_type' as outcome_type,
        _file,
        _loaded_at
    from {{ source('uk', 'crime_outcomes') }}
    qualify _loaded_at = max(_loaded_at) over (partition by _file)
)
select
    crime_id,
    (month || '-01')::date as month,
    reported_by,
    falls_within,
    longitude::double as longitude,
    latitude::double as latitude,
    location,
    lsoa_code,
    lsoa_name,
    outcome_type,
    _file,
    _loaded_at
from raw
