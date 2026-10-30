{# A DD/MM/YYYY date (Companies House), or null where it is empty. #}
{% macro dmy_date(col) %}strptime(nullif({{ col }}, ''), '%d/%m/%Y')::date{% endmacro %}
