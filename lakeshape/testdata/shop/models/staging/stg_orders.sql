select order_id, customer_id, status, {{ cents_to_units('amount_cents') }} as amount, ordered_at
from {{ source('raw', 'orders') }}
where amount_cents >= {{ var('min_amount') }}
