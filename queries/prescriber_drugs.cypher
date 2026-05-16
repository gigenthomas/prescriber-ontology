// All drugs a specific prescriber prescribed, with totals.
// Params: $npi
MATCH (p:Prescriber {external_id: $npi})-[r:prescribed]->(d:Drug)
OPTIONAL MATCH (d)-[:generic_of]->(g:GenericDrug)
RETURN
    d.canonical_label AS brand,
    g.canonical_label AS generic,
    toInteger(coalesce(r.tot_clms, 0))         AS claims,
    toFloat(coalesce(r.tot_drug_cst, 0))       AS total_cost,
    toInteger(coalesce(r.tot_benes, 0))        AS beneficiaries
ORDER BY claims DESC;
