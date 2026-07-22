# LEGACY - scheduled for deletion

These envsubst templates belong to the transitional bash bootstrap
(`bootstrap/`, see `bootstrap/LEGACY.md`). The Go control plane uses its own
templates, embedded in the binary at
`services/control-plane/internal/deploy/templates/` (Go text/template syntax,
rendered with missingkey=error and pinned by golden tests).

Changes made here have NO effect on control-plane deployments. Edit the
embedded templates instead.

Delete this directory together with `bootstrap/` once the control-plane path
is verified end-to-end.
