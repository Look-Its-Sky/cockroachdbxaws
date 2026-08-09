CREATE INDEX occurrences_rule_window
ON occurrences (region, tenant_id, incident_id, generation, event_time)
STORING (evidence_only, safe_summary);
