package middleware

import "net/http"

// RequireActingAdmin enforces the admin role plus the household policy that
// the acting profile be the account's primary profile. The fork's retained
// administrator routes use it; v2 groups build RequireActingAdminWith from
// live config.
func RequireActingAdmin(checkPrimary PrimaryProfileChecker, requiresProfile HouseholdProfileRequirement) func(http.Handler) http.Handler {
	return RequireActingAdminWith(NewActingAdminDecider(checkPrimary, requiresProfile))
}
