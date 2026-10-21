{% macro cents_to_units(col) %}({{ col }} / 100.0){% endmacro %}

{% macro orders_since(days) %}
select * from {{ ref('stg_orders') }} where ordered_at > now() - interval {{ days }} day
{% endmacro %}
