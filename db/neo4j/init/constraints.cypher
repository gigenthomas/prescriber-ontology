CREATE CONSTRAINT entity_id_unique IF NOT EXISTS
FOR (e:Entity) REQUIRE e.id IS UNIQUE;

CREATE CONSTRAINT entity_external_id IF NOT EXISTS
FOR (e:Entity) REQUIRE (e.source, e.external_id) IS UNIQUE;

CREATE INDEX entity_type IF NOT EXISTS
FOR (e:Entity) ON (e.type);

CREATE INDEX entity_canonical_label IF NOT EXISTS
FOR (e:Entity) ON (e.canonical_label);
