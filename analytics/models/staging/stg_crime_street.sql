-- data.police.uk street-level crimes, one row per crime (or anti-social
-- behaviour incident) per month and force. A month's file can be extracted
-- again from a newer archive, so only the latest load of each file is kept.
with raw as (
    -- duckpipe keeps each CSV row in record, every value as text. normalize_names
    -- turns Month and Location into _month and _location (reserved words).
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
        record->>'crime_type' as crime_type,
        record->>'last_outcome_category' as last_outcome_category,
        record->>'context' as context,
        _file,
        _loaded_at
    from {{ source('uk', 'crime_street') }}
    qualify _loaded_at = max(_loaded_at) over (partition by _file)
)
select
    crime_id,  -- empty for anti-social behaviour; not unique
    (month || '-01')::date as month,
    reported_by,
    falls_within,
    longitude::double as longitude,
    latitude::double as latitude,
    location,
    lsoa_code,
    lsoa_name,
    crime_type,
    last_outcome_category,
    context,
    _file,
    _loaded_at
from raw
