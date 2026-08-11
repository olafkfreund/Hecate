# agenix recipients.
#
# Each entry maps an encrypted file to the keys that may decrypt it. agenix
# reads only this file; the `.age` files themselves are created with
# `agenix -e <name>.age` from inside this directory.
#
# Scope, stated plainly so nobody wires this into the wrong place:
#
#   * This is for **local development** and for any NixOS host that runs
#     Hecate. Secrets are committed encrypted, and decrypted at activation
#     time by a host key or your personal key.
#
#   * CI does **not** use agenix. GitHub Actions reads repository secrets; it
#     has no age identity and should not have one — a CI runner that can
#     decrypt every developer secret is a poor trade for avoiding one settings
#     page.
#
#   * Hecate's own runtime credentials — registry pull secrets, git tokens —
#     are Kubernetes Secrets referenced by `credentialsRef`. agenix is how
#     those Secrets get *authored* declaratively on a NixOS host, not a second
#     mechanism the controller knows about.
let
  # Personal keys. Add a line per developer; run `make secrets-rekey` after.
  olaf = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAILeccj+vW/qyKepgXK0oXZfVFMf1kwmqj4uBHmjU2fz8";

  developers = [ olaf ];

  # Host keys, for machines that run Hecate and need to decrypt at activation.
  # Read one with: ssh-keyscan <host> | grep ed25519
  hosts = [ ];

  all = developers ++ hosts;
in
{
  # Nothing is encrypted yet. These declare what the e2e provider matrix
  # (#49, #100, #101) will need, so the recipients are settled before the first
  # secret exists rather than after.
  #
  # Create one with:  cd secrets && agenix -e github-token.age

  "github-token.age".publicKeys = all;
  "gitlab-token.age".publicKeys = all;
}
