# Keycloak (Datacenter profile)

Keycloak runs **once, next to the control plane**, not in every region (ADR-0009). Companies that
already have an identity provider (Okta, Entra ID, Google Workspace, and so on) can point TensoraFi at
that instead, using any OIDC issuer. Solo and Lab installs use built-in local accounts and
don't need Keycloak.

The operator is installed by helmfile when `features.keycloak` is true. These still have to be done:

1. **Database:** Keycloak needs Postgres. Use a separate database from TensoraFi's, with its own credentials stored in a Secret created out of band.
2. **Keycloak CR:** hostname `auth.<domain>`, routed through the `tensorafi` Gateway with an HTTPRoute.
3. **Realm `tensorafi`**, imported with a `KeycloakRealmImport`:
   - Client **`tensorafi-web`**: public, standard flow with PKCE (S256), redirect URI `https://app.<domain>/*`.
   - Client scope **`tensorafi-api`** with an *audience* mapper that adds `tensorafi-api` to access tokens. The API verifies `aud` (`TENSORAFI_OIDC_AUDIENCE`).
   - A *user attribute* or *organization* mapper that emits **`tensorafi_tenant`** (the tenant slug) in access tokens (`TENSORAFI_OIDC_TENANT_CLAIM`).
   - MFA required for anyone with admin roles.
4. **Point the API at it:** `TENSORAFI_AUTH_MODE=oidc` and `TENSORAFI_OIDC_ISSUER=https://auth.<domain>/realms/tensorafi`. The tensorafi chart sets these.

For local testing, `docker compose --profile auth up -d keycloak` starts Keycloak on :8180.
