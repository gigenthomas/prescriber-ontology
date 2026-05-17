package ontology.tools

import rego.v1

import data.ontology.helpers
# Pull in the actions package so the action_* branch can defer to it.
import data.ontology.actions as actions_pkg

# Tool-level authorization decision.
#
# Inputs:
#   input.tool : string  — e.g. "search_entities", "query_metric", "action_flag_for_review"
#   input.user : { sub, email, name, roles[], authenticated }
#   input.params (optional) — for action_* tools, the action's parameters
#
# Result:
#   data.ontology.tools.allow  : bool
#   data.ontology.tools.reason : string (human-readable explanation)

default allow := false
default reason := "denied: no policy rule matched (deny-by-default)"

# ─── Read-only tools ────────────────────────────────────────────────────────
#
# Anyone with any read role can call these.

read_only_tools := {
	"list_queries",
	"run_query",
	"list_metrics",
	"query_metric",
	"list_actions",
	"entity_actions",
	"entity_lineage",
	"search_entities",
	"get_entity",
	"describe_schema",
}

allow if {
	input.tool in read_only_tools
	helpers.authenticated(input.user)
	helpers.has_any(input.user, helpers.viewer_roles)
}

reason := "read tool allowed for authenticated user with read role" if {
	input.tool in read_only_tools
	helpers.authenticated(input.user)
	helpers.has_any(input.user, helpers.viewer_roles)
}

# Deny: not authenticated
reason := "denied: caller is not authenticated" if {
	input.tool in read_only_tools
	not helpers.authenticated(input.user)
}

# Deny: authenticated but no read role
reason := sprintf("denied: user has roles %v but needs one of %v", [input.user.roles, helpers.viewer_roles]) if {
	input.tool in read_only_tools
	helpers.authenticated(input.user)
	not helpers.has_any(input.user, helpers.viewer_roles)
}

# ─── Action tools — defer the decision to actions.rego ─────────────────────

allow if {
	startswith(input.tool, "action_")
	actions_pkg.allow
}

reason := actions_pkg.reason if {
	startswith(input.tool, "action_")
}
