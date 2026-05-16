STAGING_DDL = """
CREATE TABLE IF NOT EXISTS mup_dpr_staging (
    prscrbr_npi             TEXT NOT NULL,
    prscrbr_last_org_name   TEXT,
    prscrbr_first_name      TEXT,
    prscrbr_city            TEXT,
    prscrbr_state_abrvtn    TEXT,
    prscrbr_state_fips      TEXT,
    prscrbr_type            TEXT,
    prscrbr_type_src        TEXT,
    brnd_name               TEXT,
    gnrc_name               TEXT,
    tot_clms                INTEGER,
    tot_30day_fills         NUMERIC,
    tot_day_suply           INTEGER,
    tot_drug_cst            NUMERIC,
    tot_benes               INTEGER,
    ge65_sprsn_flag         TEXT,
    ge65_tot_clms           INTEGER,
    ge65_tot_30day_fills    NUMERIC,
    ge65_tot_drug_cst       NUMERIC,
    ge65_tot_day_suply      INTEGER,
    ge65_bene_sprsn_flag    TEXT,
    ge65_tot_benes          INTEGER
);
CREATE INDEX IF NOT EXISTS mup_dpr_staging_npi_idx   ON mup_dpr_staging (prscrbr_npi);
CREATE INDEX IF NOT EXISTS mup_dpr_staging_brnd_idx  ON mup_dpr_staging (brnd_name);
"""

TRUNCATE_STAGING = "TRUNCATE mup_dpr_staging;"

COPY_STAGING = """
COPY mup_dpr_staging (
    prscrbr_npi, prscrbr_last_org_name, prscrbr_first_name,
    prscrbr_city, prscrbr_state_abrvtn, prscrbr_state_fips,
    prscrbr_type, prscrbr_type_src,
    brnd_name, gnrc_name,
    tot_clms, tot_30day_fills, tot_day_suply, tot_drug_cst, tot_benes,
    ge65_sprsn_flag, ge65_tot_clms, ge65_tot_30day_fills,
    ge65_tot_drug_cst, ge65_tot_day_suply, ge65_bene_sprsn_flag, ge65_tot_benes
) FROM STDIN
"""

INSERT_PRESCRIBERS = """
INSERT INTO entity (source_id, external_id, type, canonical_label, attrs)
SELECT %(source_id)s, npi, 'Prescriber', label, attrs
FROM (
    SELECT
        prscrbr_npi AS npi,
        TRIM(COALESCE(MAX(prscrbr_first_name) || ' ', '') || MAX(prscrbr_last_org_name)) AS label,
        jsonb_strip_nulls(jsonb_build_object(
            'first_name',       MAX(prscrbr_first_name),
            'last_name_or_org', MAX(prscrbr_last_org_name),
            'state',            MAX(prscrbr_state_abrvtn),
            'city',             MAX(prscrbr_city),
            'specialty',        MAX(prscrbr_type)
        )) AS attrs
    FROM mup_dpr_staging
    GROUP BY prscrbr_npi
) p
ON CONFLICT (source_id, external_id) DO NOTHING;
"""

INSERT_DRUGS = """
INSERT INTO entity (source_id, external_id, type, canonical_label)
SELECT DISTINCT %(source_id)s, brnd_name, 'Drug', brnd_name
FROM mup_dpr_staging
WHERE brnd_name IS NOT NULL
ON CONFLICT (source_id, external_id) DO NOTHING;
"""

INSERT_GENERIC_DRUGS = """
INSERT INTO entity (source_id, external_id, type, canonical_label)
SELECT DISTINCT %(source_id)s, gnrc_name, 'GenericDrug', gnrc_name
FROM mup_dpr_staging
WHERE gnrc_name IS NOT NULL
ON CONFLICT (source_id, external_id) DO NOTHING;
"""

INSERT_SPECIALTIES = """
INSERT INTO entity (source_id, external_id, type, canonical_label)
SELECT DISTINCT %(source_id)s, prscrbr_type, 'Specialty', prscrbr_type
FROM mup_dpr_staging
WHERE prscrbr_type IS NOT NULL
ON CONFLICT (source_id, external_id) DO NOTHING;
"""

INSERT_LOCATIONS = """
INSERT INTO entity (source_id, external_id, type, canonical_label, attrs)
SELECT DISTINCT
    %(source_id)s,
    prscrbr_state_abrvtn || ':' || prscrbr_city,
    'Location',
    prscrbr_city || ', ' || prscrbr_state_abrvtn,
    jsonb_build_object('city', prscrbr_city, 'state', prscrbr_state_abrvtn)
FROM mup_dpr_staging
WHERE prscrbr_city IS NOT NULL AND prscrbr_state_abrvtn IS NOT NULL
ON CONFLICT (source_id, external_id) DO NOTHING;
"""

INSERT_PRESCRIBED = """
INSERT INTO relation (src_entity_id, dst_entity_id, predicate, attrs, source_id)
SELECT
    p.id, d.id, 'prescribed',
    jsonb_strip_nulls(jsonb_build_object(
        'tot_clms',          s.tot_clms,
        'tot_30day_fills',   s.tot_30day_fills,
        'tot_day_suply',     s.tot_day_suply,
        'tot_drug_cst',      s.tot_drug_cst,
        'tot_benes',         s.tot_benes,
        'ge65_tot_clms',     s.ge65_tot_clms,
        'ge65_tot_drug_cst', s.ge65_tot_drug_cst,
        'ge65_tot_benes',    s.ge65_tot_benes
    )),
    %(source_id)s
FROM mup_dpr_staging s
JOIN entity p ON p.source_id = %(source_id)s AND p.type = 'Prescriber' AND p.external_id = s.prscrbr_npi
JOIN entity d ON d.source_id = %(source_id)s AND d.type = 'Drug'       AND d.external_id = s.brnd_name;
"""

INSERT_GENERIC_OF = """
INSERT INTO relation (src_entity_id, dst_entity_id, predicate, source_id)
SELECT DISTINCT
    d.id, g.id, 'generic_of', %(source_id)s
FROM mup_dpr_staging s
JOIN entity d ON d.source_id = %(source_id)s AND d.type = 'Drug'        AND d.external_id = s.brnd_name
JOIN entity g ON g.source_id = %(source_id)s AND g.type = 'GenericDrug' AND g.external_id = s.gnrc_name
WHERE s.brnd_name IS NOT NULL AND s.gnrc_name IS NOT NULL;
"""

INSERT_HAS_SPECIALTY = """
INSERT INTO relation (src_entity_id, dst_entity_id, predicate, source_id)
SELECT DISTINCT
    p.id, sp.id, 'has_specialty', %(source_id)s
FROM mup_dpr_staging s
JOIN entity p  ON p.source_id  = %(source_id)s AND p.type  = 'Prescriber' AND p.external_id  = s.prscrbr_npi
JOIN entity sp ON sp.source_id = %(source_id)s AND sp.type = 'Specialty'  AND sp.external_id = s.prscrbr_type
WHERE s.prscrbr_type IS NOT NULL;
"""

INSERT_PRACTICES_IN = """
INSERT INTO relation (src_entity_id, dst_entity_id, predicate, source_id)
SELECT DISTINCT
    p.id, l.id, 'practices_in', %(source_id)s
FROM mup_dpr_staging s
JOIN entity p ON p.source_id = %(source_id)s AND p.type = 'Prescriber'
             AND p.external_id = s.prscrbr_npi
JOIN entity l ON l.source_id = %(source_id)s AND l.type = 'Location'
             AND l.external_id = s.prscrbr_state_abrvtn || ':' || s.prscrbr_city
WHERE s.prscrbr_city IS NOT NULL AND s.prscrbr_state_abrvtn IS NOT NULL;
"""

SCHEMA_TERMS = [
    ("entity_type", "Prescriber",  "Medical professional identified by NPI"),
    ("entity_type", "Drug",        "Branded drug (Brnd_Name)"),
    ("entity_type", "GenericDrug", "Generic substance (Gnrc_Name)"),
    ("entity_type", "Specialty",   "Prescriber specialty (Prscrbr_Type)"),
    ("entity_type", "Location",    "City + state of practice"),
    ("predicate", "prescribed",    "Prescriber -> Drug, with claim/cost/fill aggregates"),
    ("predicate", "generic_of",    "Drug -> GenericDrug"),
    ("predicate", "has_specialty", "Prescriber -> Specialty"),
    ("predicate", "practices_in",  "Prescriber -> Location"),
]
