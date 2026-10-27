-- One row per HM Land Registry sale (England and Wales), as it stands now,
-- keyed by postcode to dim_geography. All categories: A is a standard sale,
-- B a repossession, buy-to-let or sale to a company.
select
    transaction_id,
    date_of_transfer,
    date_trunc('month', date_of_transfer)::date as month,
    price,
    postcode,
    property_type,
    is_new_build,
    duration,
    ppd_category_type
from {{ ref('stg_price_paid') }}
