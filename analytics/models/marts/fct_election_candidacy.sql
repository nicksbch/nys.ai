-- One row per candidate standing in a constituency election of a UK general
-- election (2010 to 2024), with their party and votes. The constituency
-- election's totals are in fct_election_result (election_url).
select
    candidacy_key,
    election_url,
    general_election_polling_date as election_date,
    constituency_geographic_code as constituency_code,
    candidate_family_name,
    candidate_given_name,
    candidate_mnis_id,
    main_party_name as party_name,
    main_party_abbreviation as party_abbreviation,
    adjunct_party_name,
    candidate_is_sitting_mp as is_sitting_mp,
    candidate_is_former_mp as is_former_mp,
    candidate_is_standing_as_commons_speaker as is_speaker,
    candidate_is_standing_as_independent as is_independent,
    candidate_vote_count as votes,
    candidate_vote_share as vote_share,
    candidate_vote_change as vote_share_change,
    candidate_result_position as result_position,
    candidate_result_position = 1 as is_elected
from {{ ref('stg_parliament_candidacies') }}
