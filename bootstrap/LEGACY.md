# LEGACY - scheduled for deletion

This directory is the transitional bash bootstrap (Provider Box v1). It is
replaced by the Go control plane (`services/control-plane`), installed via
`install.sh` and operated from the web UI.

Do not add features here. Bug fixes only, and only if the control-plane path
cannot be used.

Differences to be aware of while both exist:

- The bash path deploys chrony and rsyslog as host-native systemd services;
  the control plane runs them containerized.
- The bash path does not know about the control plane's managed config at
  `/opt/provider-box/control-plane/provider-box.env`; it reads
  `config/provider-box.env` in the checkout.

Delete this directory (together with `templates/`, see `templates/LEGACY.md`)
once the control-plane path has been verified end-to-end in your environment:
install.sh, wizard, deploy all, the README verification checks, reboot
survival, and remove/redeploy of a service. When deleting, also remove the
"Transitional: the bash bootstrap" section from README.md.
