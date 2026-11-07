-- One row per constituency election of a UK general election (2010 to 2024):
-- its electorate, votes cast and winner. The election-level totals repeat on
-- every candidate's row in staging; here they are counted once. Candidates
-- are in fct_election_candidacy.
select
    election_url,
    general_election_polling_date as election_date,
    any_value(parliament_number) as parliament_number,
    any_value(constituency_geographic_code) as constituency_code,
    any_value(boundary_set_start_date) as boundaries_from,
    any_value(election_is_byelection) as is_byelection,
    any_value(electorate) as electorate,
    any_value(election_valid_vote_count) as valid_votes,
    any_value(election_invalid_vote_count) as invalid_votes,
    count(*) as candidates,
    any_value(majority) filter (where candidate_result_position = 1) as majority,
    any_value(main_party_name) filter (where candidate_result_position = 1) as winning_party_name,
    any_value(main_party_abbreviation) filter (where candidate_result_position = 1) as winning_party_abbreviation,
    any_value(election_result_summary) as result_summary
from {{ ref('stg_parliament_candidacies') }}
group by election_url, general_election_polling_date
