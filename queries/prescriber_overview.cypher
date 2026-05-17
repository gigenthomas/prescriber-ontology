// Full profile for a single prescriber: name, specialty, location, totals,
// and the top 15 prescribed drugs ordered by total cost. Useful when the
// LLM has resolved an NPI and wants a one-shot snapshot of everything that
// prescriber does.
// Params: $npi  (10-character NPI string, e.g. "1427277136")

MATCH (p:Prescriber {external_id: $npi})
OPTIONAL MATCH (p)-[:has_specialty]->(s:Specialty)
OPTIONAL MATCH (p)-[:practices_in]->(l:Location)
OPTIONAL MATCH (p)-[r:prescribed]->(d:Drug)
OPTIONAL MATCH (d)-[:generic_of]->(g:GenericDrug)

WITH p, s, l, r, d, g
ORDER BY toFloat(coalesce(r.tot_drug_cst, 0)) DESC

WITH
  p, s, l,
  collect({
    brand:         d.canonical_label,
    generic:       g.canonical_label,
    claims:        toInteger(coalesce(r.tot_clms, 0)),
    fills_30day:   toFloat(coalesce(r.tot_30day_fills, 0)),
    day_supply:    toInteger(coalesce(r.tot_day_suply, 0)),
    cost:          toFloat(coalesce(r.tot_drug_cst, 0)),
    beneficiaries: toInteger(coalesce(r.tot_benes, 0))
  }) AS drugs

RETURN
  p.external_id         AS npi,
  p.canonical_label     AS prescriber,
  s.canonical_label     AS specialty,
  l.canonical_label     AS location,
  size(drugs)                                AS unique_drugs_prescribed,
  reduce(t = 0,   d IN drugs | t + d.claims) AS total_claims,
  reduce(t = 0.0, d IN drugs | t + d.cost)   AS total_cost,
  drugs[..15]                                AS top_15_drugs;
