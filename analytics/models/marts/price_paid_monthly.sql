-- Sales and prices per month, district and property type. Standard price paid
-- transactions only (category A): B covers repossessions, buy-to-lets and
-- sales to companies.
select
    date_trunc('month', date_of_transfer)::date as month,
    district,
    property_type,
    count(*) as sales,
    median(price)::bigint as median_price,
    avg(price)::bigint as avg_price
from {{ ref('stg_price_paid') }}
where ppd_category_type = 'A'
group by all
