// Package crds embeds the generated CRD manifests so the operator image can apply them itself
// (`heirloom install-crds`). Helm installs crds/ on first install only; the chart runs this
// subcommand from a pre-upgrade hook so CRD schema drift cannot silently survive an upgrade.
// The Makefile copies config/crd into manifests/ whenever the API changes (`make manifests`).
package crds

import "embed"

//go:embed manifests/*.yaml
var FS embed.FS
