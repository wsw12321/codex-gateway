package store

// Upstream account ownership (migration 0051). An account whose
// owner_user_id is NULL is a platform account; any other account belongs to
// one tenant. Platform funding (groups, platform subscriptions, cash) and the
// Owner's account management only ever see platform accounts.

// marketplaceEnabledSQL is true while the marketplace switch is on. A missing
// settings row reads as enabled so that the stricter rules apply.
const marketplaceEnabledSQL = `COALESCE((SELECT ms.enabled FROM marketplace_settings ms WHERE ms.singleton), true)`

// platformAccountSQL is true for an account that platform funding may use.
// alias is an upstream_accounts alias, possibly from a LEFT JOIN. A candidate
// the Gateway has no row for stays eligible only while the marketplace is
// disabled: once tenants can log in, an unregistered account may be a tenant
// account whose ownership has not been committed yet.
func platformAccountSQL(alias string) string {
	return `(CASE WHEN ` + alias + `.id IS NULL THEN NOT ` + marketplaceEnabledSQL +
		` ELSE ` + alias + `.owner_user_id IS NULL AND NOT ` + alias + `.admin_suspended END)`
}
