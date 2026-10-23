-- data.police.uk stop and searches, one row per search. The CSV has no force
-- column, so it comes from the file name (YYYY-MM-<force>-stop-and-search.csv).
-- Only the latest load of each file is kept, as in stg_crime_street.
with raw as (
    -- duckpipe keeps each CSV row in record, every value as text.
    -- normalize_names turns Type into _type (a reserved word).
    select
        record->>'_type' as type,
        record->>'date' as date,
        record->>'part_of_a_policing_operation' as part_of_a_policing_operation,
        record->>'policing_operation' as policing_operation,
        record->>'latitude' as latitude,
        record->>'longitude' as longitude,
        record->>'gender' as gender,
        record->>'age_range' as age_range,
        record->>'selfdefined_ethnicity' as self_defined_ethnicity,
        record->>'officerdefined_ethnicity' as officer_defined_ethnicity,
        record->>'legislation' as legislation,
        record->>'object_of_search' as object_of_search,
        record->>'outcome' as outcome,
        record->>'outcome_linked_to_object_of_search' as outcome_linked_to_object_of_search,
        record->>'removal_of_more_than_just_outer_clothing' as removal_of_more_than_just_outer_clothing,
        _file,
        _loaded_at
    from {{ source('uk', 'crime_stop_and_search') }}
    qualify _loaded_at = max(_loaded_at) over (partition by _file)
)
select
    regexp_extract(_file, '\d{4}-\d{2}-(.+)-stop-and-search\.csv$', 1) as force,
    type,
    date::timestamptz as searched_at,
    part_of_a_policing_operation::boolean as is_part_of_policing_operation,
    policing_operation,
    latitude::double as latitude,
    longitude::double as longitude,
    gender,
    age_range,
    self_defined_ethnicity,
    officer_defined_ethnicity,
    legislation,
    object_of_search,
    outcome,
    outcome_linked_to_object_of_search::boolean as is_outcome_linked_to_object_of_search,
    removal_of_more_than_just_outer_clothing::boolean as is_more_than_outer_clothing_removed,
    _file,
    _loaded_at
from raw
