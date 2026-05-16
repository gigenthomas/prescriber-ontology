// Number of prescribers per city in the loaded state.
MATCH (l:Location)<-[:practices_in]-(p:Prescriber)
RETURN
    l.canonical_label AS location,
    count(p)          AS prescribers
ORDER BY prescribers DESC
LIMIT 25;
