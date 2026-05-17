package ontology.helpers

import rego.v1

# Shared role-check helpers used by both tools.rego and actions.rego.

# Common role sets in increasing privilege.
viewer_roles            := {"viewer", "analyst", "compliance", "senior_compliance", "admin"}
analyst_roles           := {"analyst", "compliance", "senior_compliance", "admin"}
compliance_roles        := {"compliance", "senior_compliance", "admin"}
senior_compliance_roles := {"senior_compliance", "admin"}
admin_roles             := {"admin"}

# True if the user has any role in the provided set.
has_any(user, allowed) if {
	some r in user.roles
	r in allowed
}

# True if the user is authenticated (input.user.authenticated == true).
authenticated(user) if user.authenticated == true
