// Drugs frequently co-prescribed with a target drug.
// "Co-prescribed" means the same prescriber prescribed both.
// Params: $brand
MATCH (target:Drug {external_id: $brand})<-[:prescribed]-(p:Prescriber)-[:prescribed]->(other:Drug)
WHERE other <> target
WITH other, count(DISTINCT p) AS co_prescribers
RETURN
    other.canonical_label AS brand,
    co_prescribers
ORDER BY co_prescribers DESC
LIMIT 25;
