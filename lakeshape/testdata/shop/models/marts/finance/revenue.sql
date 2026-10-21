{{ config(tags=['finance']) }}
select date_trunc('day', ordered_at) as day, sum(amount) as revenue
from {{ ref('orders') }}
group by 1
