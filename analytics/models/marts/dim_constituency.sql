-- One row per Westminster parliamentary constituency (ONS GSS code) that a
-- loaded general election was fought in: the 650 pre-2024 ones (2010
-- boundaries; Scotland 2005) and the 650 of the 2024 boundaries. Five
-- Scottish constituencies kept their boundaries and code in 2024, so there
-- are 1,295 codes. Names and other attributes are from the latest election
-- fought in each. dim_geography and dim_lsoa join to it through
-- constituency_code (2024) and constituency_pre2024_code.
select
    constituency_geographic_code as constituency_code,
    arg_max(constituency_name, election_polling_date) as constituency_name,
    arg_max(constituency_designation, election_polling_date) as designation,
    arg_max(country_geographic_code, election_polling_date) as country_code,
    arg_max(english_region_geographic_code, election_polling_date) as region_code,
    min(boundary_set_start_date) as boundaries_from,
    -- null while its boundaries are in force
    case when count(*) = count(boundary_set_end_date) then max(boundary_set_end_date) end as boundaries_to,
    bool_or(boundary_set_end_date is null) as is_current,
    bool_or(boundary_set_end_date is not null) as is_pre2024,
    count(distinct general_election_polling_date) as general_elections
from {{ ref('stg_parliament_candidacies') }}
group by constituency_geographic_code
