{{ config(materialized='incremental', unique_key='order_id') }}
select o.*, c.name as customer
from {{ ref('stg_orders') }} o
join {{ ref('stg_customers') }} c using (customer_id)
{% if is_incremental() %}
where o.ordered_at > (select max(ordered_at) from {{ this }})
{% endif %}
