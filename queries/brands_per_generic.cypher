// Generic drugs with the most distinct brand names mapped to them.
// Reveals which generics have crowded brand markets (and likely loose CMS matching).
MATCH (d:Drug)-[:generic_of]->(g:GenericDrug)
WITH g, count(DISTINCT d) AS brand_count, collect(d.canonical_label)[..5] AS sample_brands
WHERE brand_count > 1
RETURN
    g.canonical_label AS generic,
    brand_count,
    sample_brands
ORDER BY brand_count DESC
LIMIT 25;
