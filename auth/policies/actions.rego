package ontology.actions

import rego.v1

import data.ontology.helpers

# Action-level authorization decision.
#
# Inputs:
#   input.tool   : string — "action_<name>" (kept for symmetry with tools.rego)
#   input.action : string — short action name ("flag_for_review", "unflag", ...)
#   input.user   : { sub, email, name, roles[], authenticated }
#   input.params : map[string]any — the action's caller-supplied parameters
#
# Decision rules below are deliberately verbose so the policy is auditable:
# each rule pairs an allow body with a reason body using the same conditions.

default allow := false
default reason := "denied: no action policy rule matched"

# ─── add_note ──────────────────────────────────────────────────────────────
#
# Audit-only — any authenticated user with analyst role or above can attach
# notes. Not state-changing.

allow if {
	input.action == "add_note"
	helpers.authenticated(input.user)
	helpers.has_any(input.user, helpers.analyst_roles)
}

reason := "add_note allowed for analyst+" if {
	input.action == "add_note"
	helpers.authenticated(input.user)
	helpers.has_any(input.user, helpers.analyst_roles)
}

# ─── flag_for_review ───────────────────────────────────────────────────────
#
# Low/medium severity: compliance role or above.
# High severity: senior_compliance role or above (escalation gate).

allow if {
	input.action == "flag_for_review"
	input.params.severity in {"low", "medium"}
	helpers.has_any(input.user, helpers.compliance_roles)
}

reason := sprintf("flag_for_review (severity=%v) allowed for compliance+", [input.params.severity]) if {
	input.action == "flag_for_review"
	input.params.severity in {"low", "medium"}
	helpers.has_any(input.user, helpers.compliance_roles)
}

allow if {
	input.action == "flag_for_review"
	input.params.severity == "high"
	helpers.has_any(input.user, helpers.senior_compliance_roles)
}

reason := "flag_for_review (severity=high) allowed for senior_compliance+" if {
	input.action == "flag_for_review"
	input.params.severity == "high"
	helpers.has_any(input.user, helpers.senior_compliance_roles)
}

# Deny: trying high-severity flag without senior_compliance
reason := sprintf("denied: flag_for_review severity=high requires senior_compliance; user has %v", [input.user.roles]) if {
	input.action == "flag_for_review"
	input.params.severity == "high"
	not helpers.has_any(input.user, helpers.senior_compliance_roles)
}

# ─── unflag ────────────────────────────────────────────────────────────────
#
# Anyone who can flag can unflag — same compliance bar.

allow if {
	input.action == "unflag"
	helpers.has_any(input.user, helpers.compliance_roles)
}

reason := "unflag allowed for compliance+" if {
	input.action == "unflag"
	helpers.has_any(input.user, helpers.compliance_roles)
}

# ─── add_to_watchlist ──────────────────────────────────────────────────────
#
# Compliance role or above. Drug-targeted action; affects downstream alerts.

allow if {
	input.action == "add_to_watchlist"
	helpers.has_any(input.user, helpers.compliance_roles)
}

reason := sprintf("add_to_watchlist (list=%v) allowed for compliance+", [input.params.list_name]) if {
	input.action == "add_to_watchlist"
	helpers.has_any(input.user, helpers.compliance_roles)
}

# ─── Generic deny reasons (run last) ───────────────────────────────────────

reason := "denied: caller is not authenticated" if {
	startswith(input.tool, "action_")
	not helpers.authenticated(input.user)
}
