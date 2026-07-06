version: 1
metadata:
  name: provider-box-vcf-bootstrap
entries:
  - model: authentik_core.group
    state: present
    id: vcf-group
    identifiers:
      name: "${AUTHENTIK_BOOTSTRAP_GROUP_NAME}"

  - model: authentik_core.user
    state: created
    id: vcf-user
    identifiers:
      username: "${AUTHENTIK_BOOTSTRAP_USERNAME}"
    attrs:
      name: "${AUTHENTIK_BOOTSTRAP_USERNAME}"
      email: "${AUTHENTIK_BOOTSTRAP_USERNAME}@${AUTHENTIK_BOOTSTRAP_USER_EMAIL_DOMAIN}"
      password: "${AUTHENTIK_BOOTSTRAP_USER_PASSWORD}"
      groups:
        - !KeyOf vcf-group

  - model: authentik_providers_oauth2.oauth2provider
    state: present
    id: vcf-oidc
    identifiers:
      name: vcf-oidc
    attrs:
      client_type: confidential
      client_id: "${AUTHENTIK_BOOTSTRAP_CLIENT_ID}"
      client_secret: "${AUTHENTIK_BOOTSTRAP_CLIENT_SECRET}"
      authorization_flow: !Find [authentik_flows.flow, [slug, default-provider-authorization-implicit-consent]]
      invalidation_flow: !Find [authentik_flows.flow, [slug, default-provider-invalidation-flow]]
      signing_key: !Find [authentik_crypto.certificatekeypair, [name, "${AUTHENTIK_FQDN}"]]
      property_mappings:
        - !Find [authentik_providers_oauth2.scopemapping, [managed, goauthentik.io/providers/oauth2/scope-openid]]
        - !Find [authentik_providers_oauth2.scopemapping, [managed, goauthentik.io/providers/oauth2/scope-profile]]
        - !Find [authentik_providers_oauth2.scopemapping, [managed, goauthentik.io/providers/oauth2/scope-email]]
      redirect_uris:
${AUTHENTIK_BOOTSTRAP_CLIENT_REDIRECT_URIS_BLOCK}

  - model: authentik_core.application
    state: present
    identifiers:
      slug: vcf
    attrs:
      name: VCF
      provider: !KeyOf vcf-oidc
      meta_launch_url: blank://blank
