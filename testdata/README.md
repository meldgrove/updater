Recorded from GHCR on 24.11.2026 for the signature tests; the tests need no
network.

- `<image>-<version>.index.json`: the image index of `ghcr.io/meldgrove/<image>:<version>`,
  byte for byte (its digest is the image's digest).
- `<image>-<version>.bundle.json`: the Sigstore bundle that refers to it
  (actions/attest, referrers tag `sha256-<hex>`). Handbook and Backups are
  signed by the shared release workflow (meldgrove/.github); Updater 0.1.0
  by the Updater's own workflow of that time.
- `trusted_root.json`: Sigstore's public-good trusted root, from TUF
  (tuf-repo-cdn.sigstore.dev, target trusted_root.json).
