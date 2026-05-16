// Top prescribers by total claims in the loaded state.
// Aggregates the per-drug tot_clms attribute across all prescribed edges.
MATCH (p:Prescriber)-[r:prescribed]->(:Drug)
WITH p, sum(toInteger(coalesce(r.tot_clms, 0))) AS total_claims
RETURN
    p.external_id     AS npi,
    p.canonical_label AS prescriber,
    total_claims
ORDER BY total_claims DESC
LIMIT 25;
