-- ONS ward to Westminster parliamentary constituency to local authority
-- lookup (December 2019), one row per ward and constituency it falls in: a
-- ward split between constituencies has a row for each. The constituencies
-- are the pre-2024 ones (2010 boundaries; Scotland 2005), as in
-- stg_onspd_feb_2024's pcon. A one-off lookup: a newer download replaces it,
-- so only the rows of the latest loaded file are kept.
with raw as (
    -- duckpipe keeps each CSV row in record, every value as text.
    select
        record->>'wd19cd' as wd19cd,
        record->>'wd19nm' as wd19nm,
        record->>'pcon19cd' as pcon19cd,
        record->>'pcon19nm' as pcon19nm,
        record->>'lad19cd' as lad19cd,
        record->>'lad19nm' as lad19nm,
        _file,
        _loaded_at
    from {{ source('uk', 'ward_pcon19') }}
    qualify _file = arg_max(_file, _loaded_at) over ()
        and _loaded_at = max(_loaded_at) over ()
)
select
    wd19cd || '#' || pcon19cd as ward_pcon_key,
    wd19cd,
    wd19nm,
    pcon19cd,
    pcon19nm,
    lad19cd,
    lad19nm,
    _file,
    _loaded_at
from raw
