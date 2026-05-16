// Top drugs prescribed by prescribers of a given specialty, by total claims.
// Params: $specialty (e.g. "Internal Medicine", "Cardiology")
MATCH (p:Prescriber)-[:has_specialty]->(:Specialty {external_id: $specialty})
MATCH (p)-[r:prescribed]->(d:Drug)
OPTIONAL MATCH (d)-[:generic_of]->(g:GenericDrug)
WITH d, g, sum(toInteger(coalesce(r.tot_clms, 0))) AS claims
RETURN
    d.canonical_label AS brand,
    g.canonical_label AS generic,
    claims
ORDER BY claims DESC
LIMIT 25;
