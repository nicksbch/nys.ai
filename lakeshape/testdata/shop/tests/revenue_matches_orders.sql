-- Revenue per day adds up to the orders' amounts.
select r.day, r.revenue, o.amount
from {{ ref('revenue') }} r
join (select date_trunc('day', ordered_at) as day, sum(amount) as amount from {{ ref('orders') }} group by 1) o using (day)
where abs(r.revenue - o.amount) > 0.001
