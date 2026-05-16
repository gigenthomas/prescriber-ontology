// Top prescribers of a specific brand drug.
// Params: $brand (case-sensitive, matches Brnd_Name as it appears in CMS data)
MATCH (p:Prescriber)-[r:prescribed]->(d:Drug {external_id: $brand})
OPTIONAL MATCH (p)-[:has_specialty]->(s:Specialty)
RETURN
    p.canonical_label AS prescriber,
    s.canonical_label AS specialty,
    toInteger(coalesce(r.tot_clms, 0))   AS claims,
    toFloat(coalesce(r.tot_drug_cst, 0)) AS total_cost
ORDER BY claims DESC
LIMIT 25;
