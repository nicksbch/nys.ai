{# An ONS area code, or null where ONS puts a pseudo code for "not in any
   such area": E99999999 (and the S, W, N, L, M versions) for 9-character
   codes, 99ZZ00 for the old ward codes. #}
{% macro area_code(col) %}case when {{ col }} like '_99999999' or {{ col }} = '99ZZ00' then null else {{ col }} end{% endmacro %}
