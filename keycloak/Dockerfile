# Auth broker for the local demo: Keycloak with four realms imported on start.
#   staff        - broker realm for staff, upstream = mock-google (SAML)
#   customer     - broker realm for customers, upstream = mock-bankid (OIDC)
#   mock-google  - TEST IdP standing in for Google Workspace SAML
#   mock-bankid  - TEST IdP standing in for BankID via Scrive
# start-dev = in-memory H2, HTTP only. Never use this image outside local/CI.
FROM quay.io/keycloak/keycloak:26.4
COPY realms/ /opt/keycloak/data/import/
ENV KC_BOOTSTRAP_ADMIN_USERNAME=admin \
    KC_BOOTSTRAP_ADMIN_PASSWORD=admin \
    KC_HEALTH_ENABLED=true \
    KC_HOSTNAME=http://localhost:8080
ENTRYPOINT ["/opt/keycloak/bin/kc.sh"]
CMD ["start-dev", "--import-realm"]
