-- HM Land Registry price paid data, one row per transaction as it stands now.
-- The monthly files add (A), change (C) and delete (D) transactions, so the
-- latest record of each transaction wins and deleted ones are dropped.
with latest as (
    select *
    from {{ source('uk', 'price_paid') }}
    qualify row_number() over (
        partition by transaction_id order by _loaded_at desc, _file desc
    ) = 1
)
select
    trim(transaction_id, '{}') as transaction_id,
    price::bigint as price,
    date_of_transfer::timestamp::date as date_of_transfer,
    nullif(postcode, '') as postcode,
    property_type,
    old_new = 'Y' as is_new_build,
    duration,
    nullif(paon, '') as paon,
    nullif(saon, '') as saon,
    nullif(street, '') as street,
    nullif(locality, '') as locality,
    town_city,
    district,
    county,
    ppd_category_type,
    _loaded_at
from latest
where record_status <> 'D'
