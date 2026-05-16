// Specialties ranked by total drug cost they generate.
MATCH (s:Specialty)<-[:has_specialty]-(p:Prescriber)-[r:prescribed]->(:Drug)
WITH s, sum(toFloat(coalesce(r.tot_drug_cst, 0))) AS total_cost, count(DISTINCT p) AS prescribers
RETURN
    s.canonical_label AS specialty,
    prescribers,
    total_cost
ORDER BY total_cost DESC
LIMIT 25;
