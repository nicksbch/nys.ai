-- One row per live company on the Companies House register (latest monthly
-- release), keyed by its registered office postcode to dim_geography. Count
-- rows for companies per area; previous names and filing history fields stay
-- in stg_companies.
select
    companynumber as company_number,
    companyname as company_name,
    regaddresspostcode as postcode,
    regaddressposttown as post_town,
    companycategory as company_category,
    companystatus as company_status,
    countryoforigin as country_of_origin,
    incorporationdate as incorporation_date,
    -- "<code> - <description>": SIC 2007 (5 digits), a few older SIC 2003 (4)
    regexp_extract(siccodesictext_1, '^([0-9]+) - ', 1) as sic_code,
    siccodesictext_1 as sic_text,
    accountsaccountcategory as accounts_category,
    accountsnextduedate as accounts_next_due_date,
    confstmtnextduedate as confirmation_statement_next_due_date,
    mortgagesnummortoutstanding as charges_outstanding
from {{ ref('stg_companies') }}
